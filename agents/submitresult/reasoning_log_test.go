/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

// sensitiveReasoning stands for what an agent on confidential input actually
// submits: the submit tool's schema asks why the model is confident in the
// result, so the field paraphrases whatever the agent was reasoning about.
const sensitiveReasoning = "4.1.0 still references restricted-input-7742 in lib/index.ts, so the decision stands there."

// submitInputWithReasoning is a valid submission carrying sensitiveReasoning.
func submitInputWithReasoning() map[string]any {
	return map[string]any{
		"reasoning": sensitiveReasoning,
		"analysis":  map[string]any{"summary": "all good"},
	}
}

// submitLogRecord returns the single "Submitting result" record the run emitted,
// failing if there is not exactly one. Without that check every assertion below
// would pass vacuously on a run that logged nothing at all.
func submitLogRecord(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "Submitting result" {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf(`"Submitting result" records: got = %d, want = 1; the assertions below would otherwise prove nothing.`+"\nlogged:\n%s",
			len(found), logs.String())
	}
	return found[0]
}

// The submit handler logs its own line, separate from the executor's. It used to
// write the raw reasoning on every accepted submission. All three providers are
// driven: they share buildOutcome, so a provider with its own copy would leak.
func TestSubmitLog_WithholdsReasoning(t *testing.T) {
	t.Parallel()

	for name, submit := range map[string]func(t *testing.T, ctx context.Context) toolcall.SubmitOutcome[*sampleResult]{
		"claude": submitViaClaude,
		"google": submitViaGoogle,
		"openai": submitViaOpenAI,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logs := &bytes.Buffer{}
			ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(logs, nil)))

			outcome := submit(t, ctx)
			if !outcome.Accepted {
				t.Fatalf("submission: got = rejected (%#v), want = accepted; the log line is only reached on the accepted path", outcome.ToolResult)
			}

			rec := submitLogRecord(t, logs)

			// The line must still carry a signal, or this test would also pass
			// against a change that deleted the log line outright.
			if got, want := rec["reasoning_bytes"], float64(len(sensitiveReasoning)); got != want {
				t.Errorf("reasoning_bytes: got = %v, want = %v", got, want)
			}

			// Nothing in the whole sink may carry the reasoning — not the record,
			// not any other line the submission emitted.
			if strings.Contains(logs.String(), sensitiveReasoning) {
				t.Errorf("the reasoning reached the log sink in plaintext.\nlogged:\n%s", logs.String())
			}
			// A prefix check as well, so a change that logs a truncated reasoning
			// rather than the whole string still fails.
			if head := sensitiveReasoning[:20]; strings.Contains(logs.String(), head) {
				t.Errorf("a prefix of the reasoning (%q) reached the log sink.\nlogged:\n%s", head, logs.String())
			}

			// Withholding it from the log must not drop it from the outcome: the
			// trace is where the reasoning belongs, and the payload-sealing
			// tracers govern it there.
			if outcome.Reasoning != sensitiveReasoning {
				t.Errorf("outcome.Reasoning: got = %q, want the submitted reasoning preserved for the trace", outcome.Reasoning)
			}
		})
	}
}

// The surviving field is a length, not a stand-in that could widen back into
// content. An empty reasoning is still reported, so "none" differs from "gone".
func TestSubmitLog_ReasoningBytesIsAccurate(t *testing.T) {
	t.Parallel()

	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}

	for _, reasoning := range []string{"", "ok", sensitiveReasoning} {
		t.Run(strconv.Itoa(len(reasoning)), func(t *testing.T) {
			t.Parallel()

			logs := &bytes.Buffer{}
			ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(logs, nil)))
			trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")

			input := map[string]any{"reasoning": reasoning, "analysis": map[string]any{"summary": "all good"}}
			block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, input)}
			if outcome := submit.Handler(ctx, block, trace); !outcome.Accepted {
				t.Fatalf("submission: got = rejected (%#v), want = accepted", outcome.ToolResult)
			}

			rec := submitLogRecord(t, logs)
			if got, want := rec["reasoning_bytes"], float64(len(reasoning)); got != want {
				t.Errorf("reasoning_bytes: got = %v, want = %v", got, want)
			}
		})
	}
}

func submitViaClaude(t *testing.T, ctx context.Context) toolcall.SubmitOutcome[*sampleResult] {
	t.Helper()
	submit, err := ClaudeToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("ClaudeToolForResponse: %v", err)
	}
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	block := anthropic.ToolUseBlock{ID: "s1", Name: submit.Definition.Name, Input: mustMarshal(t, submitInputWithReasoning())}
	return submit.Handler(ctx, block, trace)
}

func submitViaGoogle(t *testing.T, ctx context.Context) toolcall.SubmitOutcome[*sampleResult] {
	t.Helper()
	submit, err := GoogleToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("GoogleToolForResponse: %v", err)
	}
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	call := &genai.FunctionCall{ID: "s1", Name: submit.Definition.Name, Args: submitInputWithReasoning()}
	return submit.Handler(ctx, call, trace)
}

func submitViaOpenAI(t *testing.T, ctx context.Context) toolcall.SubmitOutcome[*sampleResult] {
	t.Helper()
	submit, err := OpenAIToolForResponse[*sampleResult]()
	if err != nil {
		t.Fatalf("OpenAIToolForResponse: %v", err)
	}
	trace, _ := agenttrace.StartTrace[*sampleResult](ctx, "prompt")
	tc := openai.ChatCompletionMessageToolCall{ID: "s1"}
	tc.Function.Name = submit.Definition.Function.Name
	tc.Function.Arguments = string(mustMarshal(t, submitInputWithReasoning()))
	return submit.Handler(ctx, tc, trace)
}
