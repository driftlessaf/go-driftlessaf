/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"strings"
	"testing"
)

func TestAttributionValidate(t *testing.T) {
	t.Parallel()

	valid := Attribution{
		ProviderName: "gcp.vertex_ai",
		System:       SystemGoogleVertex,
		LogicalModel: "fast",
		Protocol:     "google-genai",
	}

	tests := []struct {
		name        string
		attribution Attribution
		wantError   string
	}{
		{name: "valid", attribution: valid},
		{name: "provider required", attribution: Attribution{System: valid.System, LogicalModel: valid.LogicalModel, Protocol: valid.Protocol}, wantError: "provider name"},
		{name: "system required", attribution: Attribution{ProviderName: valid.ProviderName, LogicalModel: valid.LogicalModel, Protocol: valid.Protocol}, wantError: "system"},
		{name: "logical model required", attribution: Attribution{ProviderName: valid.ProviderName, System: valid.System, Protocol: valid.Protocol}, wantError: "logical model"},
		{name: "protocol required", attribution: Attribution{ProviderName: valid.ProviderName, System: valid.System, LogicalModel: valid.LogicalModel}, wantError: "protocol"},
		{name: "surrounding whitespace rejected", attribution: Attribution{ProviderName: " gcp.vertex_ai", System: valid.System, LogicalModel: valid.LogicalModel, Protocol: valid.Protocol}, wantError: "leading or trailing whitespace"},
		{name: "control character rejected", attribution: Attribution{ProviderName: valid.ProviderName, System: valid.System, LogicalModel: "fast\nforged", Protocol: valid.Protocol}, wantError: "control characters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.attribution.Validate()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Validate error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}
