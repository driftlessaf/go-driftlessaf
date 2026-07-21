/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// TestWithEffortValidation checks the option accepts the shared levels,
// rejects anything else, and refuses to combine with WithThinking.
func TestWithEffortValidation(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	tests := []struct {
		name    string
		opts    []Option[*testBindable, *testResponse]
		wantErr bool
	}{
		{name: "low", opts: []Option[*testBindable, *testResponse]{WithEffort[*testBindable, *testResponse](effort.Low)}},
		{name: "high", opts: []Option[*testBindable, *testResponse]{WithEffort[*testBindable, *testResponse](effort.High)}},
		{name: "empty", opts: []Option[*testBindable, *testResponse]{WithEffort[*testBindable, *testResponse]("")}, wantErr: true},
		{name: "unknown", opts: []Option[*testBindable, *testResponse]{WithEffort[*testBindable, *testResponse]("extreme")}, wantErr: true},
		{
			name: "conflicts with WithThinking",
			opts: []Option[*testBindable, *testResponse]{
				WithEffort[*testBindable, *testResponse](effort.High),
				WithThinking[*testBindable, *testResponse](2048),
			},
			wantErr: true,
		},
		{
			name: "budget tier exceeds max_output_tokens",
			opts: []Option[*testBindable, *testResponse]{
				// The default max_output_tokens is 8192; xhigh maps to 24576 on
				// the default gemini-2.5-flash model, which must be rejected.
				WithEffort[*testBindable, *testResponse](effort.XHigh),
			},
			wantErr: true,
		},
		{
			name: "budget tier fits raised max_output_tokens",
			opts: []Option[*testBindable, *testResponse]{
				WithEffort[*testBindable, *testResponse](effort.XHigh),
				WithMaxOutputTokens[*testBindable, *testResponse](65536),
			},
		},
		{
			name: "xhigh needs no budget headroom on thinking-level models",
			opts: []Option[*testBindable, *testResponse]{
				WithEffort[*testBindable, *testResponse](effort.XHigh),
				WithModel[*testBindable, *testResponse]("gemini-3-flash-preview"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New[*testBindable, *testResponse](nil, prompt, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUsesThinkingLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  bool
	}{
		{"gemini-3-flash-preview", true},
		{"gemini-3.5-flash", true},
		{"gemini-3.1-flash-lite", true},
		{"gemini-2.5-flash", false},
		{"gemini-2.5-pro", false},
		{"gemini-1.5-pro", false},
		{"not-a-gemini-model", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			if got := usesThinkingLevel(tt.model); got != tt.want {
				t.Errorf("usesThinkingLevel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestThinkingConfigForEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		level effort.Level
		want  *genai.ThinkingConfig
	}{
		{
			name: "gemini 3 low", model: "gemini-3.5-flash", level: effort.Low,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelLow},
		},
		{
			name: "gemini 3 medium", model: "gemini-3.5-flash", level: effort.Medium,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelMedium},
		},
		{
			name: "gemini 3 high", model: "gemini-3-flash-preview", level: effort.High,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelHigh},
		},
		{
			name: "gemini 3 clamps xhigh to HIGH", model: "gemini-3.5-flash", level: effort.XHigh,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelHigh},
		},
		{
			name: "gemini 3 clamps max to HIGH", model: "gemini-3.5-flash", level: effort.Max,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelHigh},
		},
		{
			name: "gemini 2.5 low budget", model: "gemini-2.5-flash", level: effort.Low,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: ptr(int32(1024))},
		},
		{
			name: "gemini 2.5 medium budget", model: "gemini-2.5-flash", level: effort.Medium,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: ptr(int32(8192))},
		},
		{
			name: "gemini 2.5 high is dynamic", model: "gemini-2.5-pro", level: effort.High,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: ptr(int32(-1))},
		},
		{
			name: "gemini 2.5 xhigh pins the family ceiling", model: "gemini-2.5-flash", level: effort.XHigh,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: ptr(int32(24576))},
		},
		{
			name: "gemini 2.5 max pins the family ceiling", model: "gemini-2.5-flash", level: effort.Max,
			want: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: ptr(int32(24576))},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := thinkingConfigForEffort(tt.model, tt.level)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("thinkingConfigForEffort(%q, %q) mismatch (-want, +got):\n%s", tt.model, tt.level, diff)
			}
		})
	}
}
