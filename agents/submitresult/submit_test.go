/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

func TestSubmissionDoesNotLogReasoning(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(&logs, nil)))
	reasoning := rand.Text()
	args := validInput()
	args["reasoning"] = reasoning
	opts := Options[sampleResult]{PayloadFieldName: "analysis"}
	opts.setDefaults()
	trace := agenttrace.NewDefaultTracer[sampleResult](ctx).NewTrace(ctx, "fixture")
	out := buildOutcome(ctx, opts, trace, "submit", "submit_result", args)
	if !out.Accepted || out.Reasoning != reasoning {
		t.Fatalf("outcome: got accepted = %v, reasoning preserved = %v, want both true", out.Accepted, out.Reasoning == reasoning)
	}
	if strings.Contains(logs.String(), reasoning) || !strings.Contains(logs.String(), "Submitting result") {
		t.Error("submission log: want operational message without reasoning")
	}
}

func TestSubmissionCanOmitSeparateReasoning(t *testing.T) {
	t.Parallel()
	opts := OptionsForResponse[*sampleResult]()
	opts.OmitReasoning = true
	submit, err := ClaudeTool(opts)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(submit.Definition.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"reasoning"`) {
		t.Fatalf("schema still advertises a separate reasoning argument: %s", raw)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	outcome := submit.Handler(ctx, anthropic.ToolUseBlock{
		ID:   "s1",
		Name: submit.Definition.Name,
		Input: mustMarshal(t, map[string]any{
			"analysis": map[string]any{"summary": "all good"},
		}),
	}, trace)
	if !outcome.Accepted || outcome.Response == nil || outcome.Response.Summary != "all good" {
		t.Fatalf("result-only submission: got = %#v, want accepted payload", outcome)
	}
	if outcome.Reasoning != "" {
		t.Errorf("reasoning = %q, want empty when omitted", outcome.Reasoning)
	}
}

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
// skillup-skillfixer eval gate on 2026-07-20 (CI job 88351919967) and the
// manifest-gen testgen golden gate on 2026-07-24 (run 30109908781, where the
// model reproduced the malformed shape across retries and then submitted an
// artifact-less minimal payload). Coercion recovers the object instead of
// betting on a clean resubmit.
func trailingBraceInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  `{"summary":"all good"}}`,
	}
}

// trailingGarbageInput carries a stringified payload with non-delimiter
// content after the object, which coercion must still decline: the payload
// may be truncated or interleaved with prose, and only the model can fix
// that.
func trailingGarbageInput() map[string]any {
	return map[string]any{
		"reasoning": "done",
		"analysis":  `{"summary":"all good"} and that is my answer`,
	}
}

// requireRecoverableRejection asserts the trace holds exactly one tool-call
// record, that it is a recoverable rejection of the ErrParameter class, and
// that the recorded error names the cause — wantCause is a fragment of the
// same corrective hint the model received. Asserting the class alone would
// pass on a bare sentinel, which is the defect this pins: a trace that says
// only "parameter error" cannot tell an absent reasoning from a stringified
// payload from arguments that never parsed.
func requireRecoverableRejection(t *testing.T, trace *agenttrace.Trace[*sampleResult], wantCause string) {
	t.Helper()
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length: got = %d, want = 1", len(trace.ToolCalls))
	}
	tc := trace.ToolCalls[0]
	switch {
	case tc.Error == nil:
		t.Errorf("tool call error: got = nil, want = %v", ErrParameter)
	case !errors.Is(tc.Error, ErrParameter):
		t.Errorf("tool call error: got = %v, want = one wrapping %v", tc.Error, ErrParameter)
	case !strings.Contains(tc.Error.Error(), wantCause):
		t.Errorf("tool call error: got = %q, want = one naming the cause %q", tc.Error, wantCause)
	}
	if !tc.Recoverable {
		t.Errorf("tool call recoverable: got = false, want = true (rejection returned a corrective hint)")
	}
	if trace.Error != nil {
		t.Errorf("trace error: got = %v, want = nil (rejection is not terminal)", trace.Error)
	}
}

func TestClaudeSubmitCoercesTrailingBracePayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, trailingBraceInput())}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	outcome := submit.Handler(ctx, block, trace)

	if !outcome.Accepted {
		t.Fatalf("trailing-brace payload: got = rejected (%#v), want = coerced and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
}

// TestClaudeSubmitCoercesTrailingDelimiterPayloads pins the full breadth the
// coercion promises: any run of spurious closing delimiters (`}`/`]`) and
// whitespace after the payload object is tolerated, not just a single `}`.
func TestClaudeSubmitCoercesTrailingDelimiterPayloads(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tail := range []string{"}}", "]", "}]", " }\n} ", "]]}"} {
		t.Run(fmt.Sprintf("tail=%q", tail), func(t *testing.T) {
			input := map[string]any{
				"reasoning": "done",
				"analysis":  `{"summary":"all good"}` + tail,
			}
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, input)}

			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			outcome := submit.Handler(ctx, block, trace)

			if !outcome.Accepted {
				t.Fatalf("trailing %q payload: got = rejected (%#v), want = coerced and accepted", tail, outcome.ToolResult)
			}
			if got, want := outcome.Response.Summary, "all good"; got != want {
				t.Errorf("response summary: got = %q, want = %q", got, want)
			}
		})
	}
}

// TestClaudeSubmitCoercesTrailingClosingTagPayloads pins the tool-call markup
// Sonnet 5 leaks into a stringified payload: the complete object followed by
// the closing tags of the parameter and invocation it was writing. On CI job
// 108061537350 every such submit was declined, and after repeated rejections
// the model submitted `{"summary":"test","failures":[]}` placeholders, which
// were accepted and zeroed the golden-eval judges on four cases.
func TestClaudeSubmitCoercesTrailingClosingTagPayloads(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tail := range []string{
		"</analysis>\n",
		"</analysis>\n</invoke>\n",
		"\n</analysis>\n</invoke>\n",
		"</analysis>\n</submit_result>\n\n",
		"}</analysis>",
		"]</analysis>\n}",
	} {
		t.Run(fmt.Sprintf("tail=%q", tail), func(t *testing.T) {
			input := map[string]any{
				"reasoning": "done",
				"analysis":  `{"summary":"all good"}` + tail,
			}
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, input)}

			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			outcome := submit.Handler(ctx, block, trace)

			if !outcome.Accepted {
				t.Fatalf("trailing %q payload: got = rejected (%#v), want = coerced and accepted", tail, outcome.ToolResult)
			}
			if got, want := outcome.Response.Summary, "all good"; got != want {
				t.Errorf("response summary: got = %q, want = %q", got, want)
			}
		})
	}
}

// TestClaudeSubmitRejectsTrailingMarkupWithContent pins the edge of the
// closing-tag leniency: markup that opens a tag or carries text after the
// object may hold another parameter's value, so coercion still declines it.
func TestClaudeSubmitRejectsTrailingMarkupWithContent(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tail := range []string{
		"</analysis>\n<parameter name=\"reasoning\">the log shows",
		"</analysis> and that is my answer",
		"</analysis",
		"</>",
		"</ analysis>",
	} {
		t.Run(fmt.Sprintf("tail=%q", tail), func(t *testing.T) {
			input := map[string]any{
				"reasoning": "done",
				"analysis":  `{"summary":"all good"}` + tail,
			}
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, input)}

			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			outcome := submit.Handler(ctx, block, trace)

			if outcome.Accepted {
				t.Errorf("trailing %q payload: got = accepted, want = rejected", tail)
			}
			requireRecoverableRejection(t, trace, "analysis parameter must be a JSON object, got string")
		})
	}
}

func TestClaudeSubmitRejectsTrailingGarbagePayloadAsRecoverable(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, trailingGarbageInput())}
	outcome := submit.Handler(ctx, block, trace)

	if outcome.Accepted {
		t.Errorf("trailing-garbage payload: got = accepted, want = rejected")
	}
	if _, ok := outcome.ToolResult["error"]; !ok {
		t.Errorf("trailing-garbage payload: got = %#v, want = error tool result", outcome.ToolResult)
	}
	requireRecoverableRejection(t, trace, "analysis parameter must be a JSON object, got string")
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
	requireRecoverableRejection(t, trace, "reasoning parameter is required")
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
	requireRecoverableRejection(t, trace, "analysis parameter must be a JSON object, got string")
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

// TestSubmitRejectsUnparseableArgumentsAsRecoverable covers the one submit
// rejection that never reaches buildOutcome: the provider hands over
// arguments that are not JSON at all. Google is absent because its arguments
// arrive already decoded. The record must be recoverable like its siblings,
// or a no-tool agent (cve-advisor, conformance) that fumbles one submit and
// then submits cleanly fails no-errors, having no other call to count as
// work.
func TestSubmitRejectsUnparseableArgumentsAsRecoverable(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		submit, err := ClaudeToolForResponse[*sampleResult]()
		if err != nil {
			t.Fatalf("ClaudeToolForResponse: %v", err)
		}

		ctx := t.Context()
		trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

		block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: []byte(`{"reasoning":`)}
		outcome := submit.Handler(ctx, block, trace)

		if outcome.Accepted {
			t.Errorf("unparseable input: got = accepted, want = rejected")
		}
		requireRecoverableRejection(t, trace, "failed to parse tool input")
	})

	t.Run("openai", func(t *testing.T) {
		submit, err := OpenAIToolForResponse[*sampleResult]()
		if err != nil {
			t.Fatalf("OpenAIToolForResponse: %v", err)
		}

		ctx := t.Context()
		trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

		call := openai.ChatCompletionMessageToolCall{ID: "s1"}
		call.Function.Name = submit.Definition.Function.Name
		call.Function.Arguments = `{"reasoning":`
		outcome := submit.Handler(ctx, call, trace)

		if outcome.Accepted {
			t.Errorf("unparseable arguments: got = accepted, want = rejected")
		}
		requireRecoverableRejection(t, trace, "failed to parse tool arguments")
	})
}

// TestRejectedSubmitRendersDiagnosticInTrace pins the symptom rather than the
// mechanism: the rendered trace an engineer opens after a red gate must name
// what was wrong with the submit. A stringified payload is the likeliest first
// failure on a large submit schema, and the trace used to carry only
// "Error: parameter error" for it — four distinct causes collapsed into one
// string, so the reader had to re-derive the cause from the raw params by eye.
func TestRejectedSubmitRendersDiagnosticInTrace(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, malformedInput())}
	if outcome := submit.Handler(ctx, block, trace); outcome.Accepted {
		t.Fatalf("malformed payload: got = accepted, want = rejected")
	}

	rendered := trace.String()
	for _, want := range []string{
		"parameter error",
		"analysis parameter must be a JSON object, got string",
		"pass it directly as a nested JSON object",
		// The echo of what arrived: length plus the quoted content.
		`analysis arrived as a 17-byte string: "not a json object"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered trace does not name %q:\n%s", want, rendered)
		}
	}
}

// TestRejectedSubmitEchoIsBounded pins that the echo of an oversized
// stringified payload is capped. The submission that motivated the echo was
// 5,056 bytes, and the record it lands in is durable JSON plus a span
// attribute, so echoing the whole payload would trade one bad diagnostic for
// an unbounded one.
func TestRejectedSubmitEchoIsBounded(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	// A complete object followed by prose, which coercion declines (see
	// trailingGarbageInput), padded past the echo bound. The needle sits in
	// the tail so its absence proves the cap held.
	const needle = "NEEDLE_PAST_THE_BOUND"
	payload := `{"summary":"` + strings.Repeat("a", 4*payloadEchoLimit) + `"} and then ` + needle

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, map[string]any{
		"reasoning": "done",
		"analysis":  payload,
	})}
	if outcome := submit.Handler(ctx, block, trace); outcome.Accepted {
		t.Fatalf("oversized trailing-garbage payload: got = accepted, want = rejected")
	}

	recorded := trace.ToolCalls[0].Error.Error()
	if want := fmt.Sprintf("analysis arrived as a %d-byte string beginning ", len(payload)); !strings.Contains(recorded, want) {
		t.Errorf("recorded error does not report the length: got = %q, want = one containing %q", recorded, want)
	}
	if strings.Contains(recorded, needle) {
		t.Errorf("recorded error echoed past the %d-byte bound: %q", payloadEchoLimit, recorded)
	}
	// The whole record stays proportionate: the cause, the hint and a bounded
	// prefix, not a payload-sized string.
	if len(recorded) > len(payload) {
		t.Errorf("recorded error is %d bytes for a %d-byte payload; want a bounded record", len(recorded), len(payload))
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
