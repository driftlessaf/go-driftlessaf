/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudebackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
)

func TestResolveWithConfig(t *testing.T) {
	setFakeADC(t)

	tests := []struct {
		name         string
		model        string
		auth         anthropicauth.Config
		wantModel    string
		wantProvider claudeexecutor.Provider
	}{
		{
			name:         "Vertex preserves model ID",
			model:        "claude-sonnet-4-6@default",
			wantModel:    "claude-sonnet-4-6@default",
			wantProvider: claudeexecutor.ProviderVertex,
		},
		{
			name:  "Anthropic direct removes Vertex version",
			model: "claude-sonnet-4-6@default",
			auth: anthropicauth.Config{
				FederationRuleID: "fdrl_0123456789",
				OrganizationID:   "12345678-1234-1234-1234-123456789012",
			},
			wantModel:    "claude-sonnet-4-6",
			wantProvider: claudeexecutor.ProviderAnthropic,
		},
		{
			name:  "Anthropic direct preserves first-party model ID",
			model: "claude-haiku-4-5",
			auth: anthropicauth.Config{
				FederationRuleID: "fdrl_9876543210",
				OrganizationID:   "87654321-4321-4321-4321-210987654321",
			},
			wantModel:    "claude-haiku-4-5",
			wantProvider: claudeexecutor.ProviderAnthropic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(t.Context(), "my-project", "us-central1", tt.model, tt.auth)
			if got.ModelID != tt.wantModel {
				t.Errorf("ModelID: got = %q, want = %q", got.ModelID, tt.wantModel)
			}
			if got.Provider != tt.wantProvider {
				t.Errorf("Provider: got = %q, want = %q", got.Provider, tt.wantProvider)
			}
			if len(got.Messages.Options) == 0 {
				t.Error("Messages.Options: got = empty, want configured service")
			}
		})
	}
}

func TestResolveDefaultsToVertex(t *testing.T) {
	setFakeADC(t)
	t.Setenv(anthropicauth.EnvProfile, "")

	const model = "claude-sonnet-4-6@default"
	got, err := Resolve(t.Context(), "my-project", "us-central1", model)
	if err != nil {
		t.Fatalf("Resolve(): got error %v, want nil", err)
	}
	if got.ModelID != model {
		t.Errorf("ModelID: got = %q, want = %q", got.ModelID, model)
	}
	if got.Provider != claudeexecutor.ProviderVertex {
		t.Errorf("Provider: got = %q, want = %q", got.Provider, claudeexecutor.ProviderVertex)
	}
}

func setFakeADC(t *testing.T) {
	t.Helper()

	credFile := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(credFile, []byte(`{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"token"}`), 0o600); err != nil {
		t.Fatalf("writing fake ADC file: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credFile)
}

func TestResolveRejectsBrokenProfile(t *testing.T) {
	t.Setenv(anthropicauth.EnvProfile, "missing")
	t.Setenv(anthropicauth.EnvConfigDir, t.TempDir())

	_, err := Resolve(t.Context(), "my-project", "us-central1", "claude-sonnet-4-6@default")
	if err == nil {
		t.Fatal("Resolve(): got = nil, want error")
	}
	if want := "resolving anthropic auth config"; !strings.Contains(err.Error(), want) {
		t.Errorf("Resolve() error: got = %q, want substring %q", err, want)
	}
}
