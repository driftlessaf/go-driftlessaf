/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import "testing"

func TestSupportsSamplingParams(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{"opus 4.6", "claude-opus-4-6", true},
		{"opus 4.6 with region", "claude-opus-4-6@default", true},
		{"opus 4.7 drops", "claude-opus-4-7", false},
		{"opus 4.7 with region drops", "claude-opus-4-7@default", false},
		{"opus 4.7 with date drops", "claude-opus-4-7-20260101", false},
		{"opus 4.8 drops", "claude-opus-4-8", false},
		{"opus 4.8 with region drops", "claude-opus-4-8@default", false},
		{"fable 5 drops", "claude-fable-5", false},
		{"fable 5 with date drops", "claude-fable-5-20260520", false},
		{"sonnet 4.7 keeps", "claude-sonnet-4-7", true},
		{"sonnet 4.6", "claude-sonnet-4-6", true},
		{"haiku 4.5", "claude-haiku-4-5-20251001", true},
		{"unknown model", "some-random-model", true},
		{"empty string", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supportsSamplingParams(tt.model); got != tt.want {
				t.Errorf("supportsSamplingParams(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestSupportsExtendedThinkingBudget(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{"opus 4.6", "claude-opus-4-6", true},
		{"opus 4.6 with region", "claude-opus-4-6@default", true},
		{"opus 4.7 drops", "claude-opus-4-7", false},
		{"opus 4.7 with region drops", "claude-opus-4-7@default", false},
		{"opus 4.7 with date drops", "claude-opus-4-7-20260101", false},
		{"opus 4.8 drops", "claude-opus-4-8", false},
		{"opus 4.8 with region drops", "claude-opus-4-8@default", false},
		{"fable 5 drops", "claude-fable-5", false},
		{"fable 5 with date drops", "claude-fable-5-20260520", false},
		{"sonnet 4.7 keeps", "claude-sonnet-4-7", true},
		{"sonnet 4.6", "claude-sonnet-4-6", true},
		{"haiku 4.5", "claude-haiku-4-5-20251001", true},
		{"unknown model", "some-random-model", true},
		{"empty string", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supportsExtendedThinkingBudget(tt.model); got != tt.want {
				t.Errorf("supportsExtendedThinkingBudget(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
