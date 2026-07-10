/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package anthropicauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/config"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"github.com/chainguard-dev/clog"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

// Environment variables that ConfigFromEnv reads to configure the
// Anthropic-direct (first-party API + WIF) backend. See package doc and
// DEV-1839.
const (
	// EnvIdentityTokenFile is the path to a file containing the OIDC identity
	// token (JWT) to exchange for an Anthropic access token, for environments
	// where a sidecar keeps the file fresh (SourceFile). CI and Cloud Run do
	// not need it: they mint fresh tokens per exchange (SourceGitHubActions /
	// SourceGoogle). The file is re-read on every token exchange.
	EnvIdentityTokenFile = "ANTHROPIC_IDENTITY_TOKEN_FILE" //nolint:gosec // G101: env var name, not a credential
	// EnvFederationRuleID is the Anthropic OidcFederationRule ID (fdrl_*).
	EnvFederationRuleID = "ANTHROPIC_FEDERATION_RULE_ID"
	// EnvOrganizationID is the Anthropic organization UUID the rule belongs to.
	EnvOrganizationID = "ANTHROPIC_ORGANIZATION_ID"
	// EnvServiceAccountID is the optional expected-target service account
	// (svac_*) for target_type=SERVICE_ACCOUNT rules.
	EnvServiceAccountID = "ANTHROPIC_SERVICE_ACCOUNT_ID"
	// EnvWorkspaceID is the optional workspace (wrkspc_* or "default") to scope
	// the minted token to.
	EnvWorkspaceID = "ANTHROPIC_WORKSPACE_ID"

	// EnvActionsIDTokenRequestURL and EnvActionsIDTokenRequestToken are the
	// GitHub Actions OIDC endpoint and its bearer, ambient in any workflow job
	// with `id-token: write`. When present (and no source is forced), the
	// GitHub Actions source is auto-selected.
	EnvActionsIDTokenRequestURL   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	EnvActionsIDTokenRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN" //nolint:gosec // G101: env var name, not a credential

	// EnvIdentityTokenSource optionally forces the identity-token source,
	// overriding auto-detection. Valid values: SourceGitHubActions, SourceFile,
	// SourceGoogle. Leave unset to auto-detect (GitHub Actions, then file, then
	// Google — the Cloud Run / GCP default).
	EnvIdentityTokenSource = "ANTHROPIC_IDENTITY_TOKEN_SOURCE" //nolint:gosec // G101: env var name, not a credential

	// EnvProfile names an SDK config profile (configs/<name>.json under the
	// config dir) that carries the stable federation IDs — organization,
	// workspace, service account, federation rule — so a deployment ships one
	// baked, non-secret profile instead of four opaque-ID env vars. When set,
	// ConfigFromEnv loads the profile first, then overlays any ANTHROPIC_* env
	// vars on top; naming a profile commits the deployment to the
	// Anthropic-direct backend (see ConfigFromEnv). This reuses the SDK's own
	// ANTHROPIC_PROFILE name; the SDK's auto-load is bypassed (the federation
	// client passes option.WithoutEnvironmentDefaults), so anthropicauth owns
	// resolution.
	EnvProfile = "ANTHROPIC_PROFILE" //nolint:gosec // G101: env var name, not a credential
	// EnvConfigDir is the directory holding configs/<profile>.json. Empty falls
	// back to the SDK default (~/.config/anthropic). On Cloud Run, set it to
	// the ko KO_DATA_PATH so the profile ships inside the image's kodata.
	EnvConfigDir = "ANTHROPIC_CONFIG_DIR" //nolint:gosec // G101: env var name, not a credential

	// identityTokenAudience is the `aud` requested on minted identity tokens;
	// the federation rule's expected-audience matcher must equal it.
	identityTokenAudience = "https://api.anthropic.com" //nolint:gosec // G101: audience URL, not a credential
)

// IdentityTokenSource names a pluggable OIDC identity-token provider. The
// source supplies the JWT that anthropicauth exchanges for an Anthropic access
// token; it is deliberately not GitHub-specific, so DriftlessAF agents on
// Cloud Run / GCP federate via Google-issued tokens with no CI dependency.
type IdentityTokenSource string

const (
	// SourceGitHubActions mints a fresh OIDC token from the GitHub Actions
	// runner endpoint (eval CI). Requires the ACTIONS_ID_TOKEN_REQUEST_* env.
	SourceGitHubActions IdentityTokenSource = "github-actions"
	// SourceFile re-reads a JWT from IdentityTokenFile on each exchange
	// (generic; e.g. a sidecar-refreshed file). Requires IdentityTokenFile.
	SourceFile IdentityTokenSource = "file"
	// SourceGoogle mints a Google-issued OIDC token via the GCP metadata
	// server / Application Default Credentials. This is the Cloud Run default
	// and needs no CI-specific configuration; the federation rule must trust
	// the Google issuer (https://accounts.google.com).
	SourceGoogle IdentityTokenSource = "google"
)

// Config selects and parameterizes the Anthropic authentication backend. The
// zero value selects Vertex AI (unchanged prior behavior). When the
// federation fields are populated, NewClient uses the Anthropic-direct
// first-party API via Workload Identity Federation.
type Config struct {
	// IdentityTokenFile is the path to the OIDC identity-token (JWT) file.
	IdentityTokenFile string
	// FederationRuleID is the Anthropic federation rule ID (fdrl_*).
	FederationRuleID string
	// OrganizationID is the Anthropic organization UUID.
	OrganizationID string
	// ServiceAccountID is the optional target service account (svac_*).
	ServiceAccountID string
	// WorkspaceID is the optional workspace to scope the token to.
	WorkspaceID string
	// ActionsIDTokenRequestURL and ActionsIDTokenRequestToken are the GitHub
	// Actions OIDC endpoint and bearer, used by the SourceGitHubActions source.
	ActionsIDTokenRequestURL   string
	ActionsIDTokenRequestToken string
	// Source optionally forces the identity-token source, overriding
	// ResolveSource's auto-detection. Empty means auto-detect.
	Source IdentityTokenSource
}

// Configured reports whether the Anthropic-direct (first-party API + WIF)
// backend is selected. The federation rule ID and organization ID (both
// required by the SDK token exchange) must be set; otherwise NewClient falls
// back to Vertex. The identity-token source is resolved separately and
// defaults to Google (Cloud Run), so no CI-specific env is needed off-CI.
// This is the single source of truth for backend selection so the agent and
// judge paths stay in lockstep.
func (c Config) Configured() bool {
	return c.FederationRuleID != "" && c.OrganizationID != ""
}

// ResolveSource selects the identity-token source: an explicit Config.Source
// wins; otherwise auto-detect GitHub Actions (its endpoint env is present),
// then a configured token file, then Google (the GCP / Cloud Run default).
func (c Config) ResolveSource() IdentityTokenSource {
	switch {
	case c.Source != "":
		return c.Source
	case c.ActionsIDTokenRequestURL != "" && c.ActionsIDTokenRequestToken != "":
		return SourceGitHubActions
	case c.IdentityTokenFile != "":
		return SourceFile
	default:
		return SourceGoogle
	}
}

// identityProvider builds the OIDC identity-token function for the resolved
// source. The returned func is invoked by the SDK on each token exchange.
func (c Config) identityProvider() (option.IdentityTokenFunc, error) {
	switch s := c.ResolveSource(); s {
	case SourceGitHubActions:
		return githubActionsIDToken(c), nil
	case SourceFile:
		return option.IdentityTokenFile(c.IdentityTokenFile), nil
	case SourceGoogle:
		return googleIDToken(identityTokenAudience), nil
	default:
		return nil, fmt.Errorf("unknown identity token source %q", s)
	}
}

// fingerprint is a short, non-secret digest of the fields that determine the
// shared federation client. Two NewClient calls that log the same fingerprint
// resolve the same memoized client (single exchange); a difference explains a
// split.
func (c Config) fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		c.FederationRuleID, c.OrganizationID, c.ServiceAccountID,
		c.WorkspaceID, c.IdentityTokenFile, string(c.ResolveSource()),
	}, "\x00")))
	return hex.EncodeToString(h[:6])
}

// ConfigFromProfile maps an SDK config profile's federation fields into a
// Config. The profile (configs/<name>.json under dir; empty dir falls back to
// the SDK default config directory) carries the stable opaque IDs once —
// organization, workspace, service account, federation rule. The
// identity-token source is deliberately NOT read from the profile: the SDK
// profile schema models only a file source, whereas anthropicauth also mints
// github-actions and google tokens, so the source is resolved per environment
// by ResolveSource (override via EnvIdentityTokenSource). A file path, if the
// profile sets one, is carried through for SourceFile deployments.
//
// Note that the SDK's loader itself back-fills fields the profile OMITS from
// the ANTHROPIC_ORGANIZATION_ID / ANTHROPIC_WORKSPACE_ID /
// ANTHROPIC_FEDERATION_RULE_ID / ANTHROPIC_SERVICE_ACCOUNT_ID /
// ANTHROPIC_IDENTITY_TOKEN_FILE env vars, per the cross-SDK
// credential-precedence contract; fields present in the profile always win
// over the environment here. (ConfigFromEnv then overlays non-empty env vars
// on top, so through that entrypoint the environment wins either way.)
func ConfigFromProfile(dir, name string) (Config, error) {
	if dir == "" {
		dir = config.DefaultDir()
	}
	p, err := config.LoadProfile(dir, name)
	if err != nil {
		return Config{}, fmt.Errorf("loading anthropic profile %q from %q: %w", name, dir, err)
	}
	cfg := Config{
		OrganizationID: p.OrganizationID,
		WorkspaceID:    p.WorkspaceID,
	}
	if a := p.AuthenticationInfo; a != nil && a.OIDCFederation != nil {
		cfg.FederationRuleID = a.OIDCFederation.FederationRuleID
		cfg.ServiceAccountID = a.OIDCFederation.ServiceAccountID
		if it := a.OIDCFederation.IdentityToken; it != nil {
			cfg.IdentityTokenFile = it.Path
		}
	}
	return cfg, nil
}

// ConfigFromEnv builds a Config from the process environment (see the Env*
// constants). It is the binding adapter for binaries/entrypoints; the library
// constructor (NewClient) takes Config by value so it stays configurable and
// testable rather than reading the environment itself.
//
// When EnvProfile is set, the named profile supplies the stable federation IDs
// and the ANTHROPIC_* env vars overlay it, so a single value (typically the
// rule ID) can vary per deployment without a per-service profile. A named
// profile that fails to load — or that, after the env overlay, still lacks
// the federation rule ID or organization ID — is a hard error: falling
// through to the pure-env path would silently select the Vertex zero-value
// backend on a deployment that explicitly asked for a profile, and nothing
// downstream would flag the downgrade. Naming a profile therefore commits the
// deployment to the Anthropic-direct backend; the rollout lever is setting or
// unsetting EnvProfile itself, not shipping a partial profile. With no
// profile set, every field comes from its env var and the returned error is
// always nil.
func ConfigFromEnv() (Config, error) {
	var cfg Config
	name := os.Getenv(EnvProfile)
	if name != "" {
		loaded, err := ConfigFromProfile(os.Getenv(EnvConfigDir), name)
		if err != nil {
			return Config{}, err
		}
		cfg = loaded
	}
	overlayEnv(&cfg)
	if name != "" && !cfg.Configured() {
		return Config{}, fmt.Errorf("anthropic profile %q resolved without a federation rule ID and/or organization ID (after env overlay); a named profile must select the Anthropic-direct backend — unset %s to use Vertex", name, EnvProfile)
	}
	return cfg, nil
}

// overlayEnv applies ANTHROPIC_* env vars on top of cfg, overriding only the
// fields whose env var is non-empty so an unset var can't blank a profile
// value. ACTIONS_* are always read from the ambient CI environment (never
// carried in a profile).
func overlayEnv(cfg *Config) {
	setStringFromEnv(&cfg.IdentityTokenFile, EnvIdentityTokenFile)
	setStringFromEnv(&cfg.FederationRuleID, EnvFederationRuleID)
	setStringFromEnv(&cfg.OrganizationID, EnvOrganizationID)
	setStringFromEnv(&cfg.ServiceAccountID, EnvServiceAccountID)
	setStringFromEnv(&cfg.WorkspaceID, EnvWorkspaceID)
	if v := os.Getenv(EnvIdentityTokenSource); v != "" {
		cfg.Source = IdentityTokenSource(v)
	}
	cfg.ActionsIDTokenRequestURL = os.Getenv(EnvActionsIDTokenRequestURL)
	cfg.ActionsIDTokenRequestToken = os.Getenv(EnvActionsIDTokenRequestToken)
}

// setStringFromEnv overwrites *dst with the env var's value only when that
// value is non-empty.
func setStringFromEnv(dst *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	}
}

// NewClient builds the anthropic.Client for the given Vertex projectID/region
// and Config.
//
// When cfg.Configured() is true it returns a federation-authenticated client
// (first-party api.anthropic.com); otherwise it returns the existing
// Vertex-authenticated client. The Vertex path is byte-for-byte the prior
// behavior, so callers that pass an empty Config are unaffected. The selected
// backend is logged so it is unambiguous which path served a given run.
func NewClient(ctx context.Context, projectID, region string, cfg Config) anthropic.Client {
	if cfg.Configured() {
		return federationClient(ctx, cfg)
	}

	clog.InfoContext(ctx, "constructing Claude client via Vertex AI",
		"backend", "vertex",
		"project", projectID,
		"region", region,
	)
	return anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID, "https://www.googleapis.com/auth/cloud-platform"),
	)
}

// federationClient returns the Anthropic-direct client for cfg, constructing
// it at most once per distinct Config for the life of the process and reusing
// that one instance for every caller.
//
// Two properties of the construction are load-bearing; both exist because
// Anthropic's /v1/oauth/token treats each identity-token assertion as
// single-use, so any DUPLICATE auth middleware means duplicate exchanges, and
// whichever exchange replays an already-consumed assertion fails with 401
// authentication_error:
//
//  1. The client is SHARED. anthropic.NewClient prepends DefaultClientOptions
//     on every call, and when the ANTHROPIC_FEDERATION_* env vars are set its
//     credential chain installs a default federation middleware with a fresh
//     per-call token cache — so two NewClient calls (the agent and the judge
//     in an eval run) exchange independently and the second one 401s.
//  2. WithoutEnvironmentDefaults disables that ambient credential chain, so
//     the explicitly configured provider below is the ONLY auth middleware.
//     Without it the default (file-reading) middleware runs first, sets the
//     Authorization header, and the explicit provider never executes — the
//     selected token source would be configured but dead.
func federationClient(ctx context.Context, cfg Config) anthropic.Client {
	fedMu.Lock()
	defer fedMu.Unlock()
	if c, ok := fedClients[cfg]; ok {
		return c
	}
	provider, err := cfg.identityProvider()
	if err != nil {
		// A misconfigured source is a programming/deploy error, not a runtime
		// condition to paper over: returning a Vertex client would silently
		// serve the wrong backend. Surface it loudly; the exchange would fail
		// anyway. The zero client makes the misconfiguration unmistakable.
		clog.ErrorContext(ctx, "anthropic-direct identity source misconfigured; returning unusable client",
			"error", err, "source", cfg.ResolveSource())
		return anthropic.Client{}
	}
	clog.InfoContext(ctx, "constructing Claude client via Anthropic-direct (first-party API + WIF)",
		"backend", "anthropic-direct",
		"token_source", string(cfg.ResolveSource()),
		"federation_rule_id", cfg.FederationRuleID,
		"organization_id", cfg.OrganizationID,
		"service_account_id", cfg.ServiceAccountID,
		"workspace_id", cfg.WorkspaceID,
		// A stable fingerprint lets a job log confirm the agent and judge
		// resolved the SAME shared client (single exchange).
		"config_fingerprint", cfg.fingerprint(),
	)
	c := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithFederationTokenProvider(
			provider,
			option.FederationOptions{
				FederationRuleID: cfg.FederationRuleID,
				OrganizationID:   cfg.OrganizationID,
				ServiceAccountID: cfg.ServiceAccountID,
				WorkspaceID:      cfg.WorkspaceID,
			},
		),
	)
	fedClients[cfg] = c
	return c
}

var (
	fedMu      sync.Mutex
	fedClients = map[Config]anthropic.Client{}
)

// githubActionsIDToken returns an [option.IdentityTokenFunc] that mints a
// fresh GitHub Actions OIDC token (aud=https://api.anthropic.com) on every
// token exchange, via the runner's ambient ID-token endpoint. Anthropic's
// /v1/oauth/token treats each assertion as single-use, so re-presenting a
// previously exchanged JWT (e.g. from a shared token file) fails with 401
// authentication_error; minting per exchange guarantees uniqueness.
func githubActionsIDToken(cfg Config) option.IdentityTokenFunc {
	return func(ctx context.Context) (string, error) {
		u := cfg.ActionsIDTokenRequestURL + "&audience=" + url.QueryEscape(identityTokenAudience)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", fmt.Errorf("building GitHub Actions ID-token request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.ActionsIDTokenRequestToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("requesting GitHub Actions ID token: %w", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return "", fmt.Errorf("github actions ID-token endpoint returned %d: %s", resp.StatusCode, body)
		}
		var out struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("decoding GitHub Actions ID-token response: %w", err)
		}
		if out.Value == "" {
			return "", fmt.Errorf("github actions ID-token response has empty value")
		}
		return out.Value, nil
	}
}

// googleIDToken returns an [option.IdentityTokenFunc] that mints a Google
// OIDC identity token (a JWT with aud=audience) via Application Default
// Credentials / the GCP metadata server. This is the Cloud Run path: it has no
// GitHub dependency, and the underlying source mints a fresh token on each
// refresh, so the SDK's re-exchange near access-token expiry always presents a
// usable assertion. The federation rule must trust the Google issuer
// (https://accounts.google.com) and match the runtime service account.
//
// The TokenSource is created lazily on first use (it requires a context and
// can fail when no credentials are available) and cached only on SUCCESS: the
// provider lives for the whole process inside the memoized client, so latching
// a transient creation failure (e.g. a metadata-server hiccup at cold start)
// would permanently break the client. The source itself caches and refreshes
// the underlying token.
func googleIDToken(audience string) option.IdentityTokenFunc {
	var (
		mu sync.Mutex
		ts oauth2.TokenSource
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if ts == nil {
			created, err := idtoken.NewTokenSource(ctx, audience)
			if err != nil {
				return "", fmt.Errorf("creating Google ID-token source for audience %q: %w", audience, err)
			}
			ts = created
		}
		tok, err := ts.Token()
		if err != nil {
			return "", fmt.Errorf("fetching Google ID token: %w", err)
		}
		// idtoken sources carry the OIDC JWT in the AccessToken field.
		if tok.AccessToken == "" {
			return "", fmt.Errorf("empty token from Google ID-token source")
		}
		return tok.AccessToken, nil
	}
}

// ModelID maps a Vertex-style model identifier to its first-party API form by
// stripping the "@version" suffix: Vertex publisher models are addressed as
// "name@version" (e.g. "claude-sonnet-4-6@default"), which the first-party API
// rejects with a model not_found_error — its IDs never contain "@". Names
// without a suffix pass through unchanged, so this is safe to apply
// unconditionally on the Anthropic-direct path. A full Vertex<->first-party
// translation table (date-pinned versions do not always map by truncation) is
// deliberately deferred to the staging/prod rollout (DEV-1839).
func ModelID(model string) string {
	base, _, _ := strings.Cut(model, "@")
	return base
}
