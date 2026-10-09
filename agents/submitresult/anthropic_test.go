/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
)

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
