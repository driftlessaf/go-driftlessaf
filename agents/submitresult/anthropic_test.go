/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"slices"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestClaudeToolReasoningDoesNotAskForReasoning guards the wording of the
// reasoning parameter on Claude's submit tool. Claude's reasoning_extraction
// safety classifier can refuse a turn of a tool loop whose tools ask the
// model to put its reasoning in a tool input, so the description may ask for
// a short explanation and the evidence, never for why or how the model
// thought.
func TestClaudeToolReasoningDoesNotAskForReasoning(t *testing.T) {
	meta, err := ClaudeTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("ClaudeTool returned error: %v", err)
	}
	props, ok := meta.Definition.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties: got = %T, want = map[string]any", meta.Definition.InputSchema.Properties)
	}
	reasoning, ok := props["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning parameter: got = %v, want = a schema", props["reasoning"])
	}
	description, _ := reasoning["description"].(string)
	if description == "" {
		t.Fatal("reasoning description: got = empty, want = a request for a short explanation")
	}
	for _, phrase := range []string{"why", "reasoning", "thinking", "thought", "step by step"} {
		if strings.Contains(strings.ToLower(description), phrase) {
			t.Errorf("reasoning description %q contains %q: it must ask for a short explanation and the evidence, not for the model's reasoning", description, phrase)
		}
	}
}

func TestClaudeToolHandler(t *testing.T) {
	meta, err := ClaudeTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("ClaudeTool returned error: %v", err)
	}

	if meta.Definition.Name != "submit_result" {
		t.Fatalf("tool name: got = %q, want = %q", meta.Definition.Name, "submit_result")
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{
		ID:    "tool-1",
		Name:  meta.Definition.Name,
		Input: mustMarshal(t, validInput()),
	}

	outcome := meta.Handler(ctx, block, trace)
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

func TestClaudeToolNoReasoning(t *testing.T) {
	meta, err := ClaudeTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("ClaudeTool returned error: %v", err)
	}
	if meta.NoReasoning == nil {
		t.Fatal("NoReasoning: got = nil, want = a variant")
	}
	variant := *meta.NoReasoning
	if variant.NoReasoning != nil {
		t.Error("NoReasoning.NoReasoning: got = a variant, want = nil")
	}
	props, _ := variant.Definition.InputSchema.Properties.(map[string]any)
	if _, ok := props["reasoning"]; ok {
		t.Error("variant properties: got = reasoning, want = none")
	}
	if slices.Contains(variant.Definition.InputSchema.Required, "reasoning") {
		t.Error("variant required: got = reasoning, want = none")
	}

	trace, _ := agenttrace.StartTrace[*sampleResult](t.Context(), "prompt")
	outcome := variant.Handler(t.Context(), anthropic.ToolUseBlock{
		ID:    "tool-1",
		Name:  variant.Definition.Name,
		Input: mustMarshal(t, map[string]any{"analysis": map[string]any{"summary": "all good"}}),
	}, trace)
	if !outcome.Accepted {
		t.Fatalf("payload without reasoning: got = rejected (%#v), want = accepted", outcome.ToolResult)
	}

	opts := OptionsForResponse[*sampleResult]()
	opts.OmitReasoning = true
	omitted, err := ClaudeTool(opts)
	if err != nil {
		t.Fatalf("ClaudeTool with OmitReasoning returned error: %v", err)
	}
	if omitted.NoReasoning != nil {
		t.Error("NoReasoning under OmitReasoning: got = a variant, want = nil")
	}
}
