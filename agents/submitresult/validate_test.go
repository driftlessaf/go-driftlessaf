/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"strings"
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

func TestClaudeValidateToolIsNonTerminal(t *testing.T) {
	submit, validate, err := ClaudeSubmitAndValidateForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeSubmitAndValidateForResponse: %v", err)
	}
	if got, want := validate.Definition.Name, defaultValidateToolName; got != want {
		t.Fatalf("validate tool name: got = %q, want = %q", got, want)
	}
	// Submit and validate must advertise the identical input schema.
	if got, want := mustJSON(t, validate.Definition.InputSchema), mustJSON(t, submit.Definition.InputSchema); got != want {
		t.Errorf("validate schema diverges from submit:\n validate = %s\n submit   = %s", got, want)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "v1", Name: validate.Definition.Name, Input: mustMarshal(t, validInput())}
	var result *sampleResult
	resp := validate.Handler(ctx, block, trace, &result)

	if v, _ := resp["valid"].(bool); !v {
		t.Errorf("valid payload: got = %#v, want = valid:true", resp)
	}
	if result != nil {
		t.Errorf("validate must not set the final result: got = %#v, want = nil", result)
	}
}

func TestClaudeValidateToolRejectsBadPayload(t *testing.T) {
	_, validate, err := ClaudeSubmitAndValidateForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeSubmitAndValidateForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "v1", Name: validate.Definition.Name, Input: mustMarshal(t, doubleEncodedInput())}
	var result *sampleResult
	resp := validate.Handler(ctx, block, trace, &result)

	if _, ok := resp["error"]; !ok {
		t.Errorf("double-encoded payload: got = %#v, want = error", resp)
	}
	if result != nil {
		t.Errorf("rejected validate must not set result: got = %#v", result)
	}
}

func TestClaudeSubmitErrorHintsAtValidate(t *testing.T) {
	submit, _, err := ClaudeSubmitAndValidateForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeSubmitAndValidateForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, doubleEncodedInput())}
	var result *sampleResult
	resp := submit.Handler(ctx, block, trace, &result)

	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, defaultValidateToolName) {
		t.Errorf("submit error should hint at %q: got = %q", defaultValidateToolName, msg)
	}
	if result != nil {
		t.Errorf("failed submit must not set result: got = %#v", result)
	}
}

// TestSubmitHintUsesConfiguredToolNames confirms the hint references the
// configured submit and validate tool names, not a hardcoded "submit_result".
func TestSubmitHintUsesConfiguredToolNames(t *testing.T) {
	submit, err := ClaudeTool(Options[*sampleResult]{ToolName: "emit_verdict", ValidateToolName: "check_verdict"})
	if err != nil {
		t.Fatalf("ClaudeTool: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	// The default payload field name is "result" when not set via tag.
	bad := map[string]any{"reasoning": "done", "result": `{"summary":"ok"}`}
	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, bad)}
	var result *sampleResult
	resp := submit.Handler(ctx, block, trace, &result)

	msg, _ := resp["error"].(string)
	for _, want := range []string{"emit_verdict", "check_verdict"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint missing %q: got = %q", want, msg)
		}
	}
	if strings.Contains(msg, "submit_result") {
		t.Errorf("hint should not reference hardcoded submit_result: got = %q", msg)
	}
}

// TestStandaloneSubmitOmitsValidateHint confirms a submit_result built without a
// validate companion does not reference a tool that is not registered.
func TestStandaloneSubmitOmitsValidateHint(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, doubleEncodedInput())}
	var result *sampleResult
	resp := submit.Handler(ctx, block, trace, &result)

	if msg, _ := resp["error"].(string); strings.Contains(msg, defaultValidateToolName) {
		t.Errorf("standalone submit must not hint at validate: got = %q", msg)
	}
}

func TestGoogleValidateToolIsNonTerminal(t *testing.T) {
	submit, validate, err := GoogleSubmitAndValidateForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleSubmitAndValidateForResponse: %v", err)
	}
	if got, want := validate.Definition.Name, defaultValidateToolName; got != want {
		t.Fatalf("validate tool name: got = %q, want = %q", got, want)
	}
	if got, want := mustJSON(t, validate.Definition.Parameters), mustJSON(t, submit.Definition.Parameters); got != want {
		t.Errorf("validate schema diverges from submit:\n validate = %s\n submit   = %s", got, want)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := &genai.FunctionCall{ID: "v1", Name: validate.Definition.Name, Args: validInput()}
	var result *sampleResult
	resp := validate.Handler(ctx, call, trace, &result)

	if v, _ := resp.Response["valid"].(bool); !v {
		t.Errorf("valid payload: got = %#v, want = valid:true", resp.Response)
	}
	if result != nil {
		t.Errorf("validate must not set result: got = %#v", result)
	}
}

func TestOpenAIValidateToolIsNonTerminal(t *testing.T) {
	submit, validate, err := OpenAISubmitAndValidateForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAISubmitAndValidateForResponse: %v", err)
	}
	if got, want := validate.Definition.Function.Name, defaultValidateToolName; got != want {
		t.Fatalf("validate tool name: got = %q, want = %q", got, want)
	}
	if got, want := mustJSON(t, validate.Definition.Function.Parameters), mustJSON(t, submit.Definition.Function.Parameters); got != want {
		t.Errorf("validate schema diverges from submit:\n validate = %s\n submit   = %s", got, want)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	call := openai.ChatCompletionMessageToolCall{ID: "v1"}
	call.Function.Name = validate.Definition.Function.Name
	call.Function.Arguments = string(mustMarshal(t, validInput()))
	var result *sampleResult
	resp := validate.Handler(ctx, call, trace, &result)

	if v, _ := resp["valid"].(bool); !v {
		t.Errorf("valid payload: got = %#v, want = valid:true", resp)
	}
	if result != nil {
		t.Errorf("validate must not set result: got = %#v", result)
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

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	return string(mustMarshal(t, v))
}
