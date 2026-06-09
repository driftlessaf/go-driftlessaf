/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package anthropicauth

import (
	"context"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"github.com/chainguard-dev/clog"
)

// Environment variables that ConfigFromEnv reads to configure the
// Anthropic-direct (first-party API + WIF) backend. See package doc and
// DEV-1839.
const (
	// EnvIdentityTokenFile is the path to a file containing the OIDC identity
	// token (JWT) to exchange for an Anthropic access token. In CI this is a
	// GitHub Actions OIDC token minted with audience https://api.anthropic.com.
	// The file is re-read on every token exchange.
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
}

// Configured reports whether the Anthropic-direct (first-party API + WIF)
// backend is selected. Both the federation rule ID and the identity-token file
// must be set; otherwise NewClient falls back to Vertex. This is the single
// source of truth for backend selection so the agent and judge paths stay in
// lockstep.
func (c Config) Configured() bool {
	return c.FederationRuleID != "" && c.IdentityTokenFile != ""
}

// ConfigFromEnv builds a Config from the process environment (see the Env*
// constants). It is the binding adapter for binaries/entrypoints; the library
// constructor (NewClient) takes Config by value so it stays configurable and
// testable rather than reading the environment itself.
func ConfigFromEnv() Config {
	return Config{
		IdentityTokenFile: os.Getenv(EnvIdentityTokenFile),
		FederationRuleID:  os.Getenv(EnvFederationRuleID),
		OrganizationID:    os.Getenv(EnvOrganizationID),
		ServiceAccountID:  os.Getenv(EnvServiceAccountID),
		WorkspaceID:       os.Getenv(EnvWorkspaceID),
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
		clog.InfoContext(ctx, "constructing Claude client via Anthropic-direct (first-party API + WIF)",
			"backend", "anthropic-direct",
			"federation_rule_id", cfg.FederationRuleID,
			"organization_id", cfg.OrganizationID,
			"service_account_id", cfg.ServiceAccountID,
			"workspace_id", cfg.WorkspaceID,
		)
		return anthropic.NewClient(
			option.WithFederationTokenProvider(
				option.IdentityTokenFile(cfg.IdentityTokenFile),
				option.FederationOptions{
					FederationRuleID: cfg.FederationRuleID,
					OrganizationID:   cfg.OrganizationID,
					ServiceAccountID: cfg.ServiceAccountID,
					WorkspaceID:      cfg.WorkspaceID,
				},
			),
		)
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
