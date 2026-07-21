/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"encoding/json"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// TestWithEffortMapping checks the option validates the shared scale and
// clamps the levels OpenAI does not have down to "high".
func TestWithEffortMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level   effort.Level
		want    shared.ReasoningEffort
		wantErr bool
	}{
		{level: effort.Low, want: shared.ReasoningEffortLow},
		{level: effort.Medium, want: shared.ReasoningEffortMedium},
		{level: effort.High, want: shared.ReasoningEffortHigh},
		{level: effort.XHigh, want: shared.ReasoningEffortHigh},
		{level: effort.Max, want: shared.ReasoningEffortHigh},
		{level: "", wantErr: true},
		{level: "extreme", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			t.Parallel()
			e := &executor[*testRequest, testResponse]{}
			err := WithEffort[*testRequest, testResponse](tt.level)(e)
			if (err != nil) != tt.wantErr {
				t.Fatalf("WithEffort(%q) err = %v, wantErr %v", tt.level, err, tt.wantErr)
			}
			if err == nil && e.reasoningEffort != tt.want {
				t.Errorf("reasoningEffort = %q, want %q", e.reasoningEffort, tt.want)
			}
		})
	}
}

// TestReasoningEffortRequestAssembly pins the wire behavior the executor
// relies on when it places ReasoningEffort on ChatCompletionNewParams: a
// configured effort is serialized as reasoning_effort, and the zero value is
// omitted from the request entirely (omitzero) rather than sent empty.
func TestReasoningEffortRequestAssembly(t *testing.T) {
	t.Parallel()

	params := openai.ChatCompletionNewParams{
		Model:               "google/gemini-2.5-flash",
		MaxCompletionTokens: param.NewOpt(int64(64)),
		Temperature:         param.NewOpt(0.1),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("p"),
		},
	}

	unset, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("Marshal (unset): %v", err)
	}
	if strings.Contains(string(unset), "reasoning_effort") {
		t.Errorf("request with unset effort carries reasoning_effort: %s", unset)
	}

	params.ReasoningEffort = shared.ReasoningEffortHigh
	set, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("Marshal (set): %v", err)
	}
	if !strings.Contains(string(set), `"reasoning_effort":"high"`) {
		t.Errorf("request with effort set does not carry reasoning_effort=high: %s", set)
	}
}
