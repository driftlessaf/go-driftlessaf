/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package anthropicauth_test

import (
	"os"
	"path/filepath"
	"testing"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"github.com/google/go-cmp/cmp"
)

func TestConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  anthropicauth.Config
		want bool
	}{{
		name: "zero value selects vertex",
		cfg:  anthropicauth.Config{},
		want: false,
	}, {
		name: "rule and org select anthropic-direct (source resolves separately)",
		cfg: anthropicauth.Config{
			FederationRuleID: "fdrl_0123456789",
			OrganizationID:   "12345678-1234-1234-1234-123456789012",
		},
		want: true,
	}, {
		name: "rule without org selects vertex",
		cfg: anthropicauth.Config{
			FederationRuleID: "fdrl_0123456789",
		},
		want: false,
	}, {
		name: "org without rule selects vertex",
		cfg: anthropicauth.Config{
			OrganizationID: "12345678-1234-1234-1234-123456789012",
		},
		want: false,
	}, {
		name: "a token file alone does not select anthropic-direct",
		cfg: anthropicauth.Config{
			IdentityTokenFile: "/run/oidc/token",
		},
		want: false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Configured(); got != tc.want {
				t.Errorf("Configured(): got = %v, want = %v", got, tc.want)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want anthropicauth.Config
	}{{
		name: "empty environment yields zero value",
		env: map[string]string{
			anthropicauth.EnvIdentityTokenFile:          "",
			anthropicauth.EnvFederationRuleID:           "",
			anthropicauth.EnvOrganizationID:             "",
			anthropicauth.EnvServiceAccountID:           "",
			anthropicauth.EnvWorkspaceID:                "",
			anthropicauth.EnvActionsIDTokenRequestURL:   "",
			anthropicauth.EnvActionsIDTokenRequestToken: "",
			anthropicauth.EnvIdentityTokenSource:        "",
			anthropicauth.EnvProfile:                    "",
			anthropicauth.EnvConfigDir:                  "",
		},
		want: anthropicauth.Config{},
	}, {
		name: "all variables map to their fields",
		env: map[string]string{
			anthropicauth.EnvIdentityTokenFile:          "/run/oidc/token",
			anthropicauth.EnvFederationRuleID:           "fdrl_0123456789",
			anthropicauth.EnvOrganizationID:             "12345678-1234-1234-1234-123456789012",
			anthropicauth.EnvServiceAccountID:           "svac_0123456789",
			anthropicauth.EnvWorkspaceID:                "wrkspc_0123456789",
			anthropicauth.EnvActionsIDTokenRequestURL:   "https://token.invalid/?api-version=2",
			anthropicauth.EnvActionsIDTokenRequestToken: "reqtok",
			anthropicauth.EnvIdentityTokenSource:        "file",
			anthropicauth.EnvProfile:                    "",
			anthropicauth.EnvConfigDir:                  "",
		},
		want: anthropicauth.Config{
			IdentityTokenFile:          "/run/oidc/token",
			FederationRuleID:           "fdrl_0123456789",
			OrganizationID:             "12345678-1234-1234-1234-123456789012",
			ServiceAccountID:           "svac_0123456789",
			WorkspaceID:                "wrkspc_0123456789",
			ActionsIDTokenRequestURL:   "https://token.invalid/?api-version=2",
			ActionsIDTokenRequestToken: "reqtok",
			Source:                     anthropicauth.SourceFile,
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := anthropicauth.ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ConfigFromEnv() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestResolveSource(t *testing.T) {
	tests := []struct {
		name string
		cfg  anthropicauth.Config
		want anthropicauth.IdentityTokenSource
	}{{
		name: "explicit source wins over auto-detection",
		cfg: anthropicauth.Config{
			Source:                     anthropicauth.SourceGoogle,
			ActionsIDTokenRequestURL:   "https://token.invalid/?api-version=2",
			ActionsIDTokenRequestToken: "reqtok",
			IdentityTokenFile:          "/run/oidc/token",
		},
		want: anthropicauth.SourceGoogle,
	}, {
		name: "actions endpoint auto-detects github",
		cfg: anthropicauth.Config{
			ActionsIDTokenRequestURL:   "https://token.invalid/?api-version=2",
			ActionsIDTokenRequestToken: "reqtok",
			IdentityTokenFile:          "/run/oidc/token",
		},
		want: anthropicauth.SourceGitHubActions,
	}, {
		name: "token file auto-detects file when no actions endpoint",
		cfg:  anthropicauth.Config{IdentityTokenFile: "/run/oidc/token"},
		want: anthropicauth.SourceFile,
	}, {
		name: "nothing set defaults to google (cloud run)",
		cfg:  anthropicauth.Config{},
		want: anthropicauth.SourceGoogle,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResolveSource(); got != tc.want {
				t.Errorf("ResolveSource() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	t.Run("empty config returns a vertex client", func(t *testing.T) {
		// vertex.WithGoogleAuth resolves Application Default Credentials at
		// construction time, so point ADC at a static authorized_user
		// credential file. Nothing is exchanged or dialed until a request is
		// made, which this test never does.
		credFile := filepath.Join(t.TempDir(), "adc.json")
		if err := os.WriteFile(credFile, []byte(`{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"token"}`), 0o600); err != nil {
			t.Fatalf("writing fake ADC file: %v", err)
		}
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credFile)

		client := anthropicauth.NewClient(t.Context(), "test-project", "us-central1", anthropicauth.Config{})
		if client.Options == nil {
			t.Error("NewClient() with empty Config: got client with nil Options, want vertex request options")
		}
	})

	t.Run("configured config returns an anthropic-direct client", func(t *testing.T) {
		// The federation token provider reads the identity-token file lazily
		// (on each token exchange), so constructing the client makes no
		// network calls and does not touch the file.
		tokenFile := filepath.Join(t.TempDir(), "oidc-token")
		if err := os.WriteFile(tokenFile, []byte("header.payload.signature"), 0o600); err != nil {
			t.Fatalf("writing identity-token file: %v", err)
		}

		client := anthropicauth.NewClient(t.Context(), "", "", anthropicauth.Config{
			IdentityTokenFile: tokenFile,
			FederationRuleID:  "fdrl_0123456789",
			OrganizationID:    "12345678-1234-1234-1234-123456789012",
			ServiceAccountID:  "svac_0123456789",
			WorkspaceID:       "wrkspc_0123456789",
		})
		if client.Options == nil {
			t.Error("NewClient() with configured Config: got client with nil Options, want federation request options")
		}
	})
}

func TestModelID(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "vertex default suffix", model: "claude-sonnet-4-6@default", want: "claude-sonnet-4-6"},
		{name: "vertex dated suffix", model: "claude-sonnet-4@20250514", want: "claude-sonnet-4"},
		{name: "first-party id unchanged", model: "claude-sonnet-4-6", want: "claude-sonnet-4-6"},
		{name: "empty", model: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anthropicauth.ModelID(tt.model); got != tt.want {
				t.Errorf("ModelID(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

// blankAnthropicEnv blanks every env var that ConfigFromEnv — and the SDK
// profile loader underneath ConfigFromProfile, which back-fills
// profile-omitted fields from ANTHROPIC_* vars — reads, so the profile tests
// are hermetic against the ambient environment: CI jobs with id-token:write
// always carry the ACTIONS_ID_TOKEN_REQUEST_* endpoint vars, and dev machines
// may export ANTHROPIC_* vars. Both readers treat an empty value as unset.
// Tests re-set the specific vars they exercise after calling this.
func blankAnthropicEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		anthropicauth.EnvIdentityTokenFile,
		anthropicauth.EnvFederationRuleID,
		anthropicauth.EnvOrganizationID,
		anthropicauth.EnvServiceAccountID,
		anthropicauth.EnvWorkspaceID,
		anthropicauth.EnvActionsIDTokenRequestURL,
		anthropicauth.EnvActionsIDTokenRequestToken,
		anthropicauth.EnvIdentityTokenSource,
		anthropicauth.EnvProfile,
		anthropicauth.EnvConfigDir,
	} {
		t.Setenv(k, "")
	}
}

// testProfileName is the profile every test writes and loads.
const testProfileName = "evals"

// writeProfile writes an oidc_federation profile JSON to
// <dir>/configs/<testProfileName>.json, matching the layout
// config.LoadProfile reads.
func writeProfile(t *testing.T, dir, body string) {
	t.Helper()
	configs := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", configs, err)
	}
	if err := os.WriteFile(filepath.Join(configs, testProfileName+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing profile: %v", err)
	}
}

func TestConfigFromProfile(t *testing.T) {
	blankAnthropicEnv(t)
	dir := t.TempDir()
	writeProfile(t, dir, `{
  "version": "1.0",
  "organization_id": "12345678-1234-1234-1234-123456789012",
  "workspace_id": "wrkspc_0123456789",
  "authentication": {
    "type": "oidc_federation",
    "federation_rule_id": "fdrl_0123456789",
    "service_account_id": "svac_0123456789"
  }
}`)

	got, err := anthropicauth.ConfigFromProfile(dir, testProfileName)
	if err != nil {
		t.Fatalf("ConfigFromProfile() error = %v", err)
	}
	want := anthropicauth.Config{
		FederationRuleID: "fdrl_0123456789",
		OrganizationID:   "12345678-1234-1234-1234-123456789012",
		ServiceAccountID: "svac_0123456789",
		WorkspaceID:      "wrkspc_0123456789",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ConfigFromProfile() mismatch (-want, +got):\n%s", diff)
	}
	if !got.Configured() {
		t.Error("ConfigFromProfile() result is not Configured(); want federation backend selected")
	}
}

func TestConfigFromProfileMissing(t *testing.T) {
	if _, err := anthropicauth.ConfigFromProfile(t.TempDir(), "absent"); err == nil {
		t.Error("ConfigFromProfile() with no profile file: got nil error, want a load error")
	}
}

func TestConfigFromEnvProfileWithOverride(t *testing.T) {
	blankAnthropicEnv(t)
	dir := t.TempDir()
	writeProfile(t, dir, `{
  "version": "1.0",
  "organization_id": "12345678-1234-1234-1234-123456789012",
  "workspace_id": "wrkspc_0123456789",
  "authentication": {
    "type": "oidc_federation",
    "federation_rule_id": "fdrl_from_profile",
    "service_account_id": "svac_0123456789"
  }
}`)
	t.Setenv(anthropicauth.EnvConfigDir, dir)
	t.Setenv(anthropicauth.EnvProfile, testProfileName)
	// A single env var overrides just the rule ID; the rest come from the profile.
	t.Setenv(anthropicauth.EnvFederationRuleID, "fdrl_from_env")

	got, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	want := anthropicauth.Config{
		FederationRuleID: "fdrl_from_env",                        // env overrides profile
		OrganizationID:   "12345678-1234-1234-1234-123456789012", // from profile
		ServiceAccountID: "svac_0123456789",                      // from profile
		WorkspaceID:      "wrkspc_0123456789",                    // from profile
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ConfigFromEnv() with profile+override mismatch (-want, +got):\n%s", diff)
	}
}

func TestConfigFromEnvProfileLoadFailure(t *testing.T) {
	// An explicitly named profile that fails to load must be a hard error, not
	// a silent fall-through to the Vertex zero-value default: the deployment
	// asked for a profile, so a missing/corrupt one is a deploy error and
	// nothing downstream would flag the backend downgrade.
	blankAnthropicEnv(t)
	t.Setenv(anthropicauth.EnvConfigDir, t.TempDir())
	t.Setenv(anthropicauth.EnvProfile, "absent")

	if _, err := anthropicauth.ConfigFromEnv(); err == nil {
		t.Error("ConfigFromEnv() with a named but missing profile: got nil error, want a load error")
	}
}

func TestConfigFromEnvProfileUnconfigured(t *testing.T) {
	// A named profile that loads but still lacks the federation rule ID after
	// the env overlay is the same silent Vertex downgrade as a profile that
	// fails to load — the SDK loader validates only that authentication is
	// present, not the federation fields — so it must be a hard error too.
	blankAnthropicEnv(t)
	dir := t.TempDir()
	writeProfile(t, dir, `{
  "version": "1.0",
  "organization_id": "12345678-1234-1234-1234-123456789012",
  "authentication": {
    "type": "oidc_federation"
  }
}`)
	t.Setenv(anthropicauth.EnvConfigDir, dir)
	t.Setenv(anthropicauth.EnvProfile, testProfileName)

	if _, err := anthropicauth.ConfigFromEnv(); err == nil {
		t.Error("ConfigFromEnv() with a named profile lacking a federation rule ID: got nil error, want an error")
	}

	// The env overlay can still legitimately complete the profile: the rule
	// ID is the value expected to vary per deployment.
	t.Setenv(anthropicauth.EnvFederationRuleID, "fdrl_from_env")
	got, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() with the rule ID supplied by env: error = %v", err)
	}
	if !got.Configured() {
		t.Error("ConfigFromEnv() with the rule ID supplied by env: not Configured(); want federation backend selected")
	}
}

func TestConfigFromEnvProfileWithAmbientActionsEnv(t *testing.T) {
	// The production shape of the agent-eval leg: a loaded profile plus the
	// ambient ACTIONS_ID_TOKEN_REQUEST_* endpoint vars that GitHub injects on
	// any job with id-token:write. The profile supplies the stable IDs and an
	// identity-token file path; the ACTIONS_* vars must still be picked up
	// off the environment (overlayEnv assigns them unconditionally) and must
	// win source auto-detection over the profile's file path, per
	// ResolveSource's documented precedence.
	blankAnthropicEnv(t)
	dir := t.TempDir()
	writeProfile(t, dir, `{
  "version": "1.0",
  "organization_id": "12345678-1234-1234-1234-123456789012",
  "workspace_id": "wrkspc_0123456789",
  "authentication": {
    "type": "oidc_federation",
    "federation_rule_id": "fdrl_0123456789",
    "service_account_id": "svac_0123456789",
    "identity_token": {
      "source": "file",
      "path": "/var/run/secrets/oidc/token"
    }
  }
}`)
	t.Setenv(anthropicauth.EnvConfigDir, dir)
	t.Setenv(anthropicauth.EnvProfile, testProfileName)
	t.Setenv(anthropicauth.EnvActionsIDTokenRequestURL, "https://actions.example/token")
	t.Setenv(anthropicauth.EnvActionsIDTokenRequestToken, "gha-bearer")

	got, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	want := anthropicauth.Config{
		IdentityTokenFile:          "/var/run/secrets/oidc/token", // carried through from the profile
		FederationRuleID:           "fdrl_0123456789",
		OrganizationID:             "12345678-1234-1234-1234-123456789012",
		ServiceAccountID:           "svac_0123456789",
		WorkspaceID:                "wrkspc_0123456789",
		ActionsIDTokenRequestURL:   "https://actions.example/token",
		ActionsIDTokenRequestToken: "gha-bearer",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ConfigFromEnv() with profile+ambient ACTIONS env mismatch (-want, +got):\n%s", diff)
	}
	if src := got.ResolveSource(); src != anthropicauth.SourceGitHubActions {
		t.Errorf("ResolveSource() = %q, want %q (ACTIONS endpoint wins over the profile's file path)", src, anthropicauth.SourceGitHubActions)
	}

	// Without the ambient ACTIONS_* vars, the same profile falls back to its
	// identity-token file — the SourceFile carry-through deployments rely on.
	t.Setenv(anthropicauth.EnvActionsIDTokenRequestURL, "")
	t.Setenv(anthropicauth.EnvActionsIDTokenRequestToken, "")
	got, err = anthropicauth.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() without ACTIONS env: error = %v", err)
	}
	if src := got.ResolveSource(); src != anthropicauth.SourceFile {
		t.Errorf("ResolveSource() = %q, want %q (profile identity-token path selects the file source)", src, anthropicauth.SourceFile)
	}
}
