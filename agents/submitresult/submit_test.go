/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

// validInput is a well-formed {reasoning, analysis} payload for sampleResult.
func validInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  map[string]any{"summary": "all good"},
	}
}

// doubleEncodedInput is the failure mode that produced the "test" incident: the
// payload object is JSON-encoded into a string instead of passed as an object.
func doubleEncodedInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  `{"summary":"all good"}`,
	}
}

func TestClaudeSubmitRejectsBadPayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, doubleEncodedInput())}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("double-encoded payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("double-encoded payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
	if outcome.Response != nil {
		t.Errorf("rejected submit must not carry a response: got = %#v", outcome.Response)
	}
}

func TestGoogleSubmitRejectsBadPayload(t *testing.T) {
	submit, err := GoogleToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{ID: "s1", Name: submit.Definition.Name, Args: doubleEncodedInput()}
	outcome := submit.Handler(ctx, call, trace)

	if outcome.Accepted {
		t.Errorf("double-encoded payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("double-encoded payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
}

func TestOpenAISubmitAcceptsValidPayload(t *testing.T) {
	submit, err := OpenAIToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAIToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := openai.ChatCompletionMessageToolCall{ID: "s1"}
	call.Function.Name = submit.Definition.Function.Name
	call.Function.Arguments = string(mustMarshal(t, validInput()))
	outcome := submit.Handler(ctx, call, trace)

	if !outcome.Accepted {
		t.Fatalf("valid payload: got = rejected (%#v), want = accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, "done"; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
	if success, _ := outcome.ToolResult["success"].(bool); !success {
		t.Errorf("tool result: got = %#v, want = success:true", outcome.ToolResult)
	}
}

func TestOpenAISubmitRejectsBadPayload(t *testing.T) {
	submit, err := OpenAIToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAIToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := openai.ChatCompletionMessageToolCall{ID: "s1"}
	call.Function.Name = submit.Definition.Function.Name
	call.Function.Arguments = string(mustMarshal(t, doubleEncodedInput()))
	outcome := submit.Handler(ctx, call, trace)

	if outcome.Accepted {
		t.Errorf("double-encoded payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("double-encoded payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
