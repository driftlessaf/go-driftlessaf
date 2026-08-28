/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func TestAttributionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		attribution modelrouter.Attribution
		wantErr     bool
	}{
		{"Vertex AI", modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: "google.vertex"}, false},
		{"Anthropic", modelrouter.Attribution{ProviderName: "anthropic", LegacySystem: "anthropic"}, false},
		{"Bedrock", modelrouter.Attribution{ProviderName: "aws.bedrock", LegacySystem: "aws.bedrock"}, false},
		{"extension provider", modelrouter.Attribution{ProviderName: "test.provider", LegacySystem: "test.provider"}, false},
		{"empty", modelrouter.Attribution{}, true},
		{"empty provider name", modelrouter.Attribution{LegacySystem: "google.vertex"}, true},
		{"invalid provider name", modelrouter.Attribution{ProviderName: "GCP Vertex", LegacySystem: "google.vertex"}, true},
		{"empty legacy system", modelrouter.Attribution{ProviderName: "gcp.vertex_ai"}, true},
		{"invalid legacy system", modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: "google/vertex"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.attribution.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, modelrouter.ErrInvalidAttribution) {
				t.Errorf("Validate() error = %v, want ErrInvalidAttribution", err)
			}
		})
	}
}
