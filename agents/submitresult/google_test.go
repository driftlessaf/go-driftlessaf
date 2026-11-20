/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"google.golang.org/genai"
)

func TestGoogleToolHandler(t *testing.T) {
	meta, err := GoogleTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("GoogleTool returned error: %v", err)
	}

	if meta.Definition.Name != "submit_result" {
		t.Fatalf("tool name: got = %q, want = %q", meta.Definition.Name, "submit_result")
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{
		ID:   "call-1",
		Name: meta.Definition.Name,
		Args: validInput(),
	}

	outcome := meta.Handler(ctx, call, trace)
	if !outcome.Accepted {
		t.Fatalf("valid payload: got = rejected (%#v), want = accepted", outcome.ToolResult)
	}
	if success, _ := outcome.ToolResult["success"].(bool); !success {
		t.Fatalf("tool result success: got = %v, want = true (%#v)", success, outcome.ToolResult)
	}
	if outcome.Response == nil {
		t.Fatal("response: got = nil, want = non-nil")
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, "done"; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
}

// GoogleTool carries a variant without the reasoning parameter, whose handler
// accepts a payload with no reasoning, and builds none when the options omit
// reasoning already.
func TestGoogleToolNoReasoning(t *testing.T) {
	meta, err := GoogleTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("GoogleTool returned error: %v", err)
	}
	if meta.NoReasoning == nil {
		t.Fatal("NoReasoning: got = nil, want = a variant")
	}
	variant := *meta.NoReasoning
	if variant.NoReasoning != nil {
		t.Error("NoReasoning.NoReasoning: got = a variant, want = nil")
	}
	if _, ok := variant.Definition.Parameters.Properties["reasoning"]; ok {
		t.Error("variant properties: got = reasoning, want = none")
	}
	if slices.Contains(variant.Definition.Parameters.Required, "reasoning") {
		t.Error("variant required: got = reasoning, want = none")
	}

	trace, _ := agenttrace.StartTrace[*sampleResult](t.Context(), "prompt")
	outcome := variant.Handler(t.Context(), &genai.FunctionCall{
		ID:   "call-1",
		Name: variant.Definition.Name,
		Args: map[string]any{"analysis": map[string]any{"summary": "all good"}},
	}, trace)
	if !outcome.Accepted {
		t.Fatalf("payload without reasoning: got = rejected (%#v), want = accepted", outcome.ToolResult)
	}

	opts := OptionsForResponse[*sampleResult]()
	opts.OmitReasoning = true
	omitted, err := GoogleTool(opts)
	if err != nil {
		t.Fatalf("GoogleTool with OmitReasoning returned error: %v", err)
	}
	if omitted.NoReasoning != nil {
		t.Error("NoReasoning under OmitReasoning: got = a variant, want = nil")
	}
}
