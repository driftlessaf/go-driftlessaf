/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import "testing"

func TestClaudeTemperatureOptions(t *testing.T) {
	tests := []struct {
		name    string
		modelID string
		want    int
	}{
		{name: "sonnet 5 adaptive", modelID: "claude-sonnet-5", want: 0},
		{name: "sonnet 5 vertex version", modelID: "claude-sonnet-5@20260801", want: 0},
		{name: "sonnet 5 bedrock", modelID: "anthropic.claude-sonnet-5", want: 0},
		{name: "opus 4.8 adaptive", modelID: "claude-opus-4-8", want: 0},
		{name: "sonnet 4.6 sampling", modelID: "claude-sonnet-4-6", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(claudeTemperatureOptions[*testRequest, *testResponse](tt.modelID)); got != tt.want {
				t.Errorf("claudeTemperatureOptions(%q): got = %d, want = %d", tt.modelID, got, tt.want)
			}
		})
	}
}
