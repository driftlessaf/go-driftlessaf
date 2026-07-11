/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
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
		t.Fatalf("unexpected tool name: %s", meta.Definition.Name)
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
		t.Fatalf("expected success tool result: %#v", outcome.ToolResult)
	}
	if outcome.Response == nil {
		t.Fatal("expected response to be set")
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, "done"; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
}
