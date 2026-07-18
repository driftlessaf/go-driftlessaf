/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"testing"

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
		effort  string
		wantErr bool
	}{
		{"low", false},
		{"medium", false},
		{"high", false},
		{"xhigh", false},
		{"max", false},
		{"", true},        // must be one of the levels; unset means "don't call the option"
		{"XHIGH", true},   // case-sensitive
		{"extreme", true}, // not a level
	}
	for _, tt := range tests {
		t.Run(tt.effort, func(t *testing.T) {
			t.Parallel()
			exec, err := New[*testBindable, *testResponse](anthropic.Client{}, prompt,
				WithEffort[*testBindable, *testResponse](tt.effort))
			if (err != nil) != tt.wantErr {
				t.Fatalf("WithEffort(%q) err = %v, wantErr %v", tt.effort, err, tt.wantErr)
			}
			if err == nil {
				e := exec.(*executor[*testBindable, *testResponse])
				if string(e.effort) != tt.effort {
					t.Errorf("effort = %q, want %q", e.effort, tt.effort)
				}
			}
		})
	}
}

// TestWithEffortAssemblesOutputConfig confirms the effort rides into
// output_config.effort, and that leaving it unset omits the field.
func TestWithEffortAssemblesOutputConfig(t *testing.T) {
	t.Parallel()

	set := newTestExecutor(t, WithEffort[*testBindable, *testResponse]("xhigh"))
	params, _, err := set.assembleParams("p", "", nil)
	if err != nil {
		t.Fatalf("assembleParams: %v", err)
	}
	if params.OutputConfig.Effort != anthropic.OutputConfigEffortXhigh {
		t.Errorf("OutputConfig.Effort = %q, want %q", params.OutputConfig.Effort, anthropic.OutputConfigEffortXhigh)
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
