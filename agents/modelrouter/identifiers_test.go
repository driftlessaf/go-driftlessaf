/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func TestProviderValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider modelrouter.Provider
		wantErr  bool
	}{
		{"shipped Vertex AI", modelrouter.ProviderVertexAI, false},
		{"shipped Anthropic", modelrouter.ProviderAnthropic, false},
		{"shipped Bedrock", modelrouter.ProviderAWSBedrock, false},
		{"extension with hyphen", "test-provider", false},
		{"extension with namespace", "example.provider_v2", false},
		{"empty", "", true},
		{"uppercase", "Vertex", true},
		{"space", "test provider", true},
		{"slash", "test/provider", true},
		{"leading separator", "-provider", true},
		{"trailing separator", "provider-", true},
		{"non-ASCII", "provïder", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.provider.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestProtocolValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		protocol modelrouter.Protocol
		wantErr  bool
	}{
		{modelrouter.ProtocolGoogleGenAI, false},
		{modelrouter.ProtocolAnthropicMessages, false},
		{modelrouter.ProtocolOpenAIChatCompletions, false},
		{"", true},
		{"anthropic-responses", true},
	}
	for _, tt := range tests {
		t.Run(string(tt.protocol), func(t *testing.T) {
			t.Parallel()
			err := tt.protocol.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
