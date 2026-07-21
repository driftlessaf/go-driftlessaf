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

// doubleEncodedInput is a common model mistake: the payload object is
// JSON-encoded into a string instead of passed as an object. The handlers
// coerce it back into an object rather than burning a retry turn.
func doubleEncodedInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  `{"summary":"all good"}`,
	}
}

// malformedInput carries a payload string that does not contain a JSON
// object, so coercion cannot recover it and the submit must be rejected.
func malformedInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  "not a json object",
	}
}

// trailingBraceInput carries a stringified payload that is a complete JSON
// object followed by one spurious closing brace — the shape that failed the
// skillup-skillfixer eval gate on 2026-07-20 (CI job 88351919967): coercion
// declines the trailing data, so the submit is rejected with a corrective
// hint and the model resubmits.
func trailingBraceInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  `{"summary":"all good"}}`,
	}
}

// requireRecoverableRejection asserts the trace holds exactly one tool-call
// record and that it is a recoverable rejection carrying wantErr.
func requireRecoverableRejection(t *testing.T, trace *agenttrace.Trace[*sampleResult], wantErr string) {
	t.Helper()
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length: got = %d, want = 1", len(trace.ToolCalls))
	}
	tc := trace.ToolCalls[0]
	if tc.Error == nil || tc.Error.Error() != wantErr {
		t.Errorf("tool call error: got = %v, want = %q", tc.Error, wantErr)
	}
	if !tc.Recoverable {
		t.Errorf("tool call recoverable: got = false, want = true (rejection returned a corrective hint)")
	}
	if trace.Error != nil {
		t.Errorf("trace error: got = %v, want = nil (rejection is not terminal)", trace.Error)
	}
}

func TestClaudeSubmitRejectsTrailingBracePayloadAsRecoverable(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, trailingBraceInput())}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("trailing-brace payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("trailing-brace payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
	requireRecoverableRejection(t, trace, "parameter error")
}

func TestClaudeSubmitRejectsMissingReasoningAsRecoverable(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, map[string]any{
		"analysis": map[string]any{"summary": "all good"},
	})}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("missing reasoning: got = accepted, want = rejected")
	}
	requireRecoverableRejection(t, trace, "parameter error")
}

func TestClaudeSubmitRejectsUnparseablePayloadAsRecoverable(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	// The payload is an object but does not unmarshal into sampleResult
	// (summary must be a string), so parsePayload rejects it after the
	// parameter checks pass.
	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, map[string]any{
		"reasoning": "done",
		"analysis":  map[string]any{"summary": 42},
	})}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("unparseable payload: got = accepted, want = rejected")
	}
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length: got = %d, want = 1", len(trace.ToolCalls))
	}
	if tc := trace.ToolCalls[0]; !tc.Recoverable {
		t.Errorf("tool call recoverable: got = false, want = true (rejection returned a corrective hint)")
	}
}

func TestClaudeSubmitCoercesStringifiedPayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, doubleEncodedInput())}
	outcome := submit.Handler(ctx, block, trace)

	if !outcome.Accepted {
		t.Fatalf("double-encoded payload: got = rejected (%#v), want = coerced and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
}

func TestClaudeSubmitRejectsMalformedPayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, malformedInput())}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("malformed payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("malformed payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
	if outcome.Response != nil {
		t.Errorf("rejected submit must not carry a response: got = %#v", outcome.Response)
	}
	requireRecoverableRejection(t, trace, "parameter error")
}

func TestGoogleSubmitCoercesStringifiedPayload(t *testing.T) {
	submit, err := GoogleToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{ID: "s1", Name: submit.Definition.Name, Args: doubleEncodedInput()}
	outcome := submit.Handler(ctx, call, trace)

	if !outcome.Accepted {
		t.Fatalf("double-encoded payload: got = rejected (%#v), want = coerced and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
}

func TestGoogleSubmitRejectsMalformedPayload(t *testing.T) {
	submit, err := GoogleToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{ID: "s1", Name: submit.Definition.Name, Args: malformedInput()}
	outcome := submit.Handler(ctx, call, trace)

	if outcome.Accepted {
		t.Errorf("malformed payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("malformed payload: got = %#v, want = error tool result", outcome.ToolResult)
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

func TestOpenAISubmitCoercesStringifiedPayload(t *testing.T) {
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

	if !outcome.Accepted {
		t.Fatalf("double-encoded payload: got = rejected (%#v), want = coerced and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
}

func TestOpenAISubmitRejectsMalformedPayload(t *testing.T) {
	submit, err := OpenAIToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAIToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := openai.ChatCompletionMessageToolCall{ID: "s1"}
	call.Function.Name = submit.Definition.Function.Name
	call.Function.Arguments = string(mustMarshal(t, malformedInput()))
	outcome := submit.Handler(ctx, call, trace)

	if outcome.Accepted {
		t.Errorf("malformed payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("malformed payload: got = %#v, want = error tool result", outcome.ToolResult)
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
