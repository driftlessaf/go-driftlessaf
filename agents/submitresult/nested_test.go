/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"bytes"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

// nestedHint is the fragment of the corrective hint that names where a
// declined nested payload went.
const nestedHint = `reasoning contains a <parameter name="analysis"> block`

// nestedInput is a submit whose payload was written inside reasoning: the
// model closed reasoning with the wrong tag, so the payload parameter's
// opening tag and the payload became reasoning text after prose.
func nestedInput(prose, payload string) map[string]any {
	return map[string]any{
		"reasoning": prose + "</reasoning>\n" + `<parameter name="analysis">` + payload,
	}
}

// TestClaudeSubmitRecoversPayloadNestedInReasoning pins the shapes prod
// submits arrived in: the payload follows the payload parameter's opener
// inside reasoning, after `</reasoning>`, `</parameter>` or no tag at all,
// and is trailed by nothing or by closing tags. The recovered submit must
// carry the payload, and only the prose as its reasoning, which is what the
// well-formed resubmit carried.
func TestClaudeSubmitRecoversPayloadNestedInReasoning(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}
	prose := rand.Text()

	for _, tc := range []struct {
		name      string
		reasoning string
		want      string
	}{{
		name:      "closed with reasoning tag",
		reasoning: prose + "</reasoning>\n" + `<parameter name="analysis">{"summary":"all good"}`,
		want:      prose,
	}, {
		name:      "closed with parameter tag",
		reasoning: prose + "</parameter>\n" + `<parameter name="analysis">{"summary":"all good"}`,
		want:      prose,
	}, {
		name:      "no closing tag",
		reasoning: prose + "\n" + `<parameter name="analysis">{"summary":"all good"}`,
		want:      prose,
	}, {
		name:      "no prose",
		reasoning: `<parameter name="analysis">{"summary":"all good"}`,
		want:      "",
	}, {
		name:      "prose ending in other markup keeps it",
		reasoning: prose + " <b>done</b></reasoning>\n" + `<parameter name="analysis">{"summary":"all good"}`,
		want:      prose + " <b>done</b>",
	}, {
		name:      "trailing parameter tag",
		reasoning: prose + "</reasoning>\n" + `<parameter name="analysis">{"summary":"all good"}</parameter>`,
		want:      prose,
	}, {
		name:      "trailing parameter and invoke tags",
		reasoning: prose + "</reasoning>\n" + `<parameter name="analysis">` + "\n" + `{"summary":"all good"}` + "\n</parameter>\n</invoke>\n",
		want:      prose,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(&logs, nil)))
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, map[string]any{"reasoning": tc.reasoning})}

			outcome := submit.Handler(ctx, block, trace)

			if !outcome.Accepted {
				t.Fatalf("nested payload: got = rejected (%#v), want = recovered and accepted", outcome.ToolResult)
			}
			if got, want := outcome.Response.Summary, "all good"; got != want {
				t.Errorf("response summary: got = %q, want = %q", got, want)
			}
			if got := outcome.Reasoning; got != tc.want {
				t.Errorf("reasoning: got = %q, want = %q", got, tc.want)
			}
			if !strings.Contains(logs.String(), "Recovered submit payload nested in reasoning") {
				t.Errorf("logs: got = %s, want = the recovery warning", logs.String())
			}
			if strings.Contains(logs.String(), prose) {
				t.Error("logs: got = the reasoning text, want = no reasoning content")
			}
		})
	}
}

// TestClaudeSubmitRejectsUnrecoverableNestedPayload pins the edge of the
// recovery: a nested payload that is cut short, carries text after the
// object, appears twice, follows another parameter's opener or is not an
// object stays rejected, and the hint says where the payload went, so the
// resubmit is one cheap turn.
func TestClaudeSubmitRejectsUnrecoverableNestedPayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tc := range []struct {
		name  string
		input map[string]any
	}{{
		name:  "truncated object",
		input: nestedInput("done", `{"summary":"all go`),
	}, {
		name:  "text after the object",
		input: nestedInput("done", `{"summary":"all good"} and that is my answer`),
	}, {
		name:  "two openers",
		input: nestedInput("done", `{"summary":"one"}</parameter>`+"\n"+`<parameter name="analysis">{"summary":"two"}`),
	}, {
		name:  "another parameter's opener in the prose",
		input: map[string]any{"reasoning": "done</reasoning>\n" + `<parameter name="verdict">ok</parameter>` + "\n" + `<parameter name="analysis">{"summary":"all good"}`},
	}, {
		name:  "not an object",
		input: nestedInput("done", `"all good"`),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, tc.input)}

			outcome := submit.Handler(ctx, block, trace)

			if outcome.Accepted {
				t.Fatalf("unrecoverable nested payload: got = accepted, want = rejected")
			}
			hint, _ := outcome.ToolResult["error"].(string)
			for _, want := range []string{"analysis parameter is required", nestedHint, "send the analysis as its own parameter"} {
				if !strings.Contains(hint, want) {
					t.Errorf("tool result error: got = %q, want = one containing %q", hint, want)
				}
			}
			requireRecoverableRejection(t, trace, nestedHint)
		})
	}
}

// TestClaudeSubmitNestedRecoveryNeedsAnAbsentPayload pins what the recovery
// does not touch. A payload parameter that arrived, even mistyped, is the
// model's answer, and an opener for another parameter is not this payload,
// so both keep the plain hint.
func TestClaudeSubmitNestedRecoveryNeedsAnAbsentPayload(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, tc := range []struct {
		name      string
		input     map[string]any
		wantCause string
	}{{
		name: "mistyped payload present",
		input: map[string]any{
			"reasoning": "done</reasoning>\n" + `<parameter name="analysis">{"summary":"all good"}`,
			"analysis":  42,
		},
		wantCause: "analysis parameter must be a JSON object",
	}, {
		name: "opener for another parameter",
		input: map[string]any{
			"reasoning": "done</reasoning>\n" + `<parameter name="verdict">{"summary":"all good"}`,
		},
		wantCause: "analysis parameter is required",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, tc.input)}

			outcome := submit.Handler(ctx, block, trace)

			if outcome.Accepted {
				t.Fatalf("got = accepted, want = rejected")
			}
			if hint, _ := outcome.ToolResult["error"].(string); strings.Contains(hint, "inside reasoning") {
				t.Errorf("tool result error: got = %q, want = the plain hint", hint)
			}
			requireRecoverableRejection(t, trace, tc.wantCause)
		})
	}
}

// TestClaudeSubmitNestedPayloadParsesLikeADirectOne pins that a recovered
// payload meets the same parse as one sent as the payload parameter: a
// payload that does not unmarshal into the response is rejected with the
// same error either way.
func TestClaudeSubmitNestedPayloadParsesLikeADirectOne(t *testing.T) {
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	rejection := func(input map[string]any) string {
		t.Helper()
		ctx := t.Context()
		trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
		block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, input)}
		outcome := submit.Handler(ctx, block, trace)
		if outcome.Accepted {
			t.Fatalf("payload with a mistyped summary: got = accepted, want = rejected")
		}
		hint, _ := outcome.ToolResult["error"].(string)
		return hint
	}

	direct := rejection(map[string]any{
		"reasoning": "done",
		"analysis":  map[string]any{"summary": 42},
	})
	nested := rejection(nestedInput("done", `{"summary":42}`))

	if !strings.Contains(direct, "failed to unmarshal payload") {
		t.Fatalf("direct rejection: got = %q, want = the unmarshal error", direct)
	}
	if nested != direct {
		t.Errorf("nested rejection: got = %q, want = %q (the direct submit's)", nested, direct)
	}
}

// TestSubmitNestedRecoveryNeedsSeparateReasoning pins that a tool which
// omits the reasoning parameter never reads one: a reasoning key the schema
// did not offer is not mined for a payload.
func TestSubmitNestedRecoveryNeedsSeparateReasoning(t *testing.T) {
	opts := OptionsForResponse[*sampleResult]()
	opts.OmitReasoning = true
	submit, err := ClaudeTool(opts)
	if err != nil {
		t.Fatalf("ClaudeTool: %v", err)
	}

	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, nestedInput("done", `{"summary":"all good"}`))}

	if outcome := submit.Handler(ctx, block, trace); outcome.Accepted {
		t.Fatalf("got = accepted (%#v), want = rejected", outcome.Response)
	}
	requireRecoverableRejection(t, trace, "analysis parameter is required")
}

func TestGoogleSubmitRecoversPayloadNestedInReasoning(t *testing.T) {
	submit, err := GoogleToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleToolForResponse: %v", err)
	}

	prose := rand.Text()
	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	call := &genai.FunctionCall{ID: "s1", Name: submit.Definition.Name, Args: nestedInput(prose, `{"summary":"all good"}`)}
	outcome := submit.Handler(ctx, call, trace)

	if !outcome.Accepted {
		t.Fatalf("nested payload: got = rejected (%#v), want = recovered and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, prose; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
}

func TestOpenAISubmitRecoversPayloadNestedInReasoning(t *testing.T) {
	submit, err := OpenAIToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAIToolForResponse: %v", err)
	}

	prose := rand.Text()
	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	call := openai.ChatCompletionMessageToolCall{ID: "s1"}
	call.Function.Name = submit.Definition.Function.Name
	call.Function.Arguments = string(mustMarshal(t, nestedInput(prose, `{"summary":"all good"}`)))
	outcome := submit.Handler(ctx, call, trace)

	if !outcome.Accepted {
		t.Fatalf("nested payload: got = rejected (%#v), want = recovered and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, prose; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
}

func TestResponsesSubmitRecoversPayloadNestedInReasoning(t *testing.T) {
	submit, err := ResponsesTool(OptionsForResponse[*sampleResult]())
	if err != nil {
		t.Fatalf("ResponsesTool: %v", err)
	}

	prose := rand.Text()
	ctx := t.Context()
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	call := toolcall.ToolCall{ID: "s1", Name: submit.Definition.Name, Args: nestedInput(prose, `{"summary":"all good"}`)}
	outcome := submit.Handler(ctx, call, trace)

	if !outcome.Accepted {
		t.Fatalf("nested payload: got = rejected (%#v), want = recovered and accepted", outcome.ToolResult)
	}
	if got, want := outcome.Response.Summary, "all good"; got != want {
		t.Errorf("response summary: got = %q, want = %q", got, want)
	}
	if got, want := outcome.Reasoning, prose; got != want {
		t.Errorf("reasoning: got = %q, want = %q", got, want)
	}
}
