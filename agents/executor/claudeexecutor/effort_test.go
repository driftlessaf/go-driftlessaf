/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestWithEffortValidation checks the option accepts the documented levels and
// rejects anything else.
func TestWithEffortValidation(t *testing.T) {
	t.Parallel()

	prompt, err := promptbuilder.NewPrompt("p")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	tests := []struct {
		effort  effort.Level
		wantErr bool
	}{
		{effort.Low, false},
		{effort.Medium, false},
		{effort.High, false},
		{effort.XHigh, false},
		{effort.Max, false},
		{"", true},        // must be one of the levels; unset means "don't call the option"
		{"XHIGH", true},   // case-sensitive
		{"extreme", true}, // not a level
	}
	for _, tt := range tests {
		t.Run(string(tt.effort), func(t *testing.T) {
			t.Parallel()
			exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
				WithEffort[*testBindable, *testResponse](tt.effort))
			if (err != nil) != tt.wantErr {
				t.Fatalf("WithEffort(%q) err = %v, wantErr %v", tt.effort, err, tt.wantErr)
			}
			if err == nil {
				e := exec.(*executor[*testBindable, *testResponse])
				if string(e.effort) != string(tt.effort) {
					t.Errorf("effort = %q, want %q", e.effort, tt.effort)
				}
			}
		})
	}
}

// TestWithEffortAssemblesOutputConfig confirms the effort rides into
// output_config.effort resolved for the configured model, and that leaving
// it unset omits the field.
func TestWithEffortAssemblesOutputConfig(t *testing.T) {
	t.Parallel()

	set := newTestExecutor(t,
		WithModel[*testBindable, *testResponse]("claude-sonnet-5"),
		WithEffort[*testBindable, *testResponse](effort.XHigh))
	params, _, err := set.assembleParams("p", "", nil)
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if params.OutputConfig.Effort != anthropic.OutputConfigEffortXhigh {
		t.Errorf("OutputConfig.Effort = %q, want %q", params.OutputConfig.Effort, anthropic.OutputConfigEffortXhigh)
	}

	// Models that predate xhigh clamp it to high rather than sending a value
	// the API rejects.
	clamped := newTestExecutor(t,
		WithModel[*testBindable, *testResponse]("claude-sonnet-4-6@default"),
		WithEffort[*testBindable, *testResponse](effort.XHigh))
	clampedParams, _, err := clamped.assembleParams("p", "", nil)
	if err != nil {
		t.Fatalf("assembleParams (clamped): %v", err)
	}
	if clampedParams.OutputConfig.Effort != anthropic.OutputConfigEffortHigh {
		t.Errorf("OutputConfig.Effort = %q, want %q (xhigh clamped for pre-xhigh model)", clampedParams.OutputConfig.Effort, anthropic.OutputConfigEffortHigh)
	}

	// The default test model (Sonnet 4) predates the effort parameter
	// entirely, so the field is dropped rather than sent.
	dropped := newTestExecutor(t, WithEffort[*testBindable, *testResponse](effort.High))
	droppedParams, _, err := dropped.assembleParams("p", "", nil)
	if err != nil {
		t.Fatalf("assembleParams (dropped): %v", err)
	}
	if droppedParams.OutputConfig.Effort != "" {
		t.Errorf("OutputConfig.Effort = %q, want empty (dropped for pre-effort model)", droppedParams.OutputConfig.Effort)
	}

	unset := newTestExecutor(t)
	unsetParams, _, err := unset.assembleParams("p", "", nil)
	if err != nil {
		t.Fatalf("assembleParams (unset): %v", err)
	}
	if unsetParams.OutputConfig.Effort != "" {
		t.Errorf("OutputConfig.Effort = %q, want empty (omitted) when WithEffort is not applied", unsetParams.OutputConfig.Effort)
	}
}

// TestEffortForModel pins the per-model resolution table: exact on models
// with the full scale, xhigh clamped to high on models that predate it,
// dropped on models without effort support, and full pass-through for
// unknown (future) models.
func TestEffortForModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		level anthropic.OutputConfigEffort
		want  anthropic.OutputConfigEffort
	}{
		// Full-scale models: exact.
		{"claude-sonnet-5", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortXhigh},
		{"claude-opus-4-7", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortXhigh},
		{"claude-opus-4-8", anthropic.OutputConfigEffortMax, anthropic.OutputConfigEffortMax},
		{"claude-fable-5", anthropic.OutputConfigEffortLow, anthropic.OutputConfigEffortLow},
		// Pre-xhigh models: xhigh clamps to high, the rest is exact.
		{"claude-sonnet-4-6@default", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortHigh},
		{"claude-sonnet-4-6@default", anthropic.OutputConfigEffortMax, anthropic.OutputConfigEffortMax},
		{"claude-sonnet-4-6@default", anthropic.OutputConfigEffortHigh, anthropic.OutputConfigEffortHigh},
		{"claude-opus-4-5@20251101", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortHigh},
		{"claude-opus-4-6", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortHigh},
		// Pre-effort models: dropped.
		{"claude-sonnet-4@20250514", anthropic.OutputConfigEffortHigh, ""},
		{"claude-sonnet-4-5@20250929", anthropic.OutputConfigEffortLow, ""},
		{"claude-haiku-4-5@20251001", anthropic.OutputConfigEffortMedium, ""},
		{"claude-opus-4-1", anthropic.OutputConfigEffortMax, ""},
		{"claude-3-7-sonnet@20250219", anthropic.OutputConfigEffortHigh, ""},
		// Unknown (future) models keep the newest surface: pass through.
		{"claude-sonnet-6", anthropic.OutputConfigEffortXhigh, anthropic.OutputConfigEffortXhigh},
		{"claude-opus-5-0", anthropic.OutputConfigEffortMax, anthropic.OutputConfigEffortMax},
		// Unset stays unset.
		{"claude-sonnet-5", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+string(tt.level), func(t *testing.T) {
			t.Parallel()
			if got := effortForModel(tt.model, tt.level); got != tt.want {
				t.Errorf("effortForModel(%q, %q) = %q, want %q", tt.model, tt.level, got, tt.want)
			}
		})
	}
}
