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
		name: "rule and token file select anthropic-direct",
		cfg: anthropicauth.Config{
			FederationRuleID:  "fdrl_0123456789",
			IdentityTokenFile: "/run/oidc/token",
		},
		want: true,
	}, {
		name: "rule without token file selects vertex",
		cfg: anthropicauth.Config{
			FederationRuleID: "fdrl_0123456789",
		},
		want: false,
	}, {
		name: "token file without rule selects vertex",
		cfg: anthropicauth.Config{
			IdentityTokenFile: "/run/oidc/token",
		},
		want: false,
	}, {
		name: "optional fields alone do not select anthropic-direct",
		cfg: anthropicauth.Config{
			OrganizationID:   "12345678-1234-1234-1234-123456789012",
			ServiceAccountID: "svac_0123456789",
			WorkspaceID:      "wrkspc_0123456789",
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
			anthropicauth.EnvIdentityTokenFile: "",
			anthropicauth.EnvFederationRuleID:  "",
			anthropicauth.EnvOrganizationID:    "",
			anthropicauth.EnvServiceAccountID:  "",
			anthropicauth.EnvWorkspaceID:       "",
		},
		want: anthropicauth.Config{},
	}, {
		name: "all variables map to their fields",
		env: map[string]string{
			anthropicauth.EnvIdentityTokenFile: "/run/oidc/token",
			anthropicauth.EnvFederationRuleID:  "fdrl_0123456789",
			anthropicauth.EnvOrganizationID:    "12345678-1234-1234-1234-123456789012",
			anthropicauth.EnvServiceAccountID:  "svac_0123456789",
			anthropicauth.EnvWorkspaceID:       "wrkspc_0123456789",
		},
		want: anthropicauth.Config{
			IdentityTokenFile: "/run/oidc/token",
			FederationRuleID:  "fdrl_0123456789",
			OrganizationID:    "12345678-1234-1234-1234-123456789012",
			ServiceAccountID:  "svac_0123456789",
			WorkspaceID:       "wrkspc_0123456789",
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := anthropicauth.ConfigFromEnv()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ConfigFromEnv() mismatch (-want, +got):\n%s", diff)
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
