/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"testing"
)

type captureResult struct{ Answer string }

// TestCaptureTrace_ForwardsAndCaptures drives a trace through the teed tracer
// exactly as an executor would (NewTrace via StartTrace, then RecordTrace on
// completion) and asserts both that the inner tracer still observed the trace
// (CloudEvent/logging emission must not be silently dropped) and that the
// accessor returns the completed trace.
func TestCaptureTrace_ForwardsAndCaptures(t *testing.T) {
	var forwarded *Trace[captureResult]
	fake := ByCode[captureResult](func(tr *Trace[captureResult]) {
		forwarded = tr
	})

	ctx := WithTracer[captureResult](t.Context(), fake)
	ctx, captured := CaptureTrace[captureResult](ctx)

	if got := captured(); got != nil {
		t.Errorf("captured() before any trace completes = %v, want nil", got)
	}

	trace, done := StartTrace[captureResult](ctx, "prompt")
	trace.Reasoning = []ReasoningContent{{Thinking: "Looked at the issue."}}
	done(captureResult{Answer: "ok"}, nil)

	if forwarded == nil {
		t.Fatal("inner tracer never received RecordTrace — CaptureTrace must forward, not replace, the original tracer")
	}
	if forwarded != trace {
		t.Error("inner tracer received a different trace than the one completed")
	}
	if got := captured(); got != trace {
		t.Errorf("captured() = %v, want the completed trace", got)
	}
}

// TestCaptureTrace_LastWins confirms a retried run supersedes its predecessor.
func TestCaptureTrace_LastWins(t *testing.T) {
	ctx, captured := CaptureTrace[captureResult](t.Context())

	_, done1 := StartTrace[captureResult](ctx, "first attempt")
	done1(captureResult{}, nil)

	t2, done2 := StartTrace[captureResult](ctx, "second attempt")
	done2(captureResult{}, nil)

	if got := captured(); got != t2 {
		t.Errorf("captured() = %v, want the second attempt's trace", got)
	}
}

// TestCaptureTrace_IndependentCaptures confirms two captures never share state.
func TestCaptureTrace_IndependentCaptures(t *testing.T) {
	ctx1, captured1 := CaptureTrace[captureResult](t.Context())
	_, captured2 := CaptureTrace[captureResult](t.Context())

	_, done := StartTrace[captureResult](ctx1, "prompt")
	done(captureResult{}, nil)

	if captured1() == nil {
		t.Error("captured1() = nil, want the completed trace")
	}
	if got := captured2(); got != nil {
		t.Errorf("captured2() = %v, want nil — it must not observe capture one's trace", got)
	}
}

// TestAttachToolCallReasoning covers the executor-side merge of the model's
// universal reasoning argument onto the recorded tool call.
func TestAttachToolCallReasoning(t *testing.T) {
	ctx, _ := CaptureTrace[captureResult](t.Context())
	trace, done := StartTrace[captureResult](ctx, "prompt")

	tc := trace.StartToolCall("call-1", "edit_file", map[string]any{"path": "a.go"})
	tc.Complete(map[string]any{"ok": true}, nil)

	// Merge onto the completed record.
	trace.AttachToolCallReasoning("call-1", "Fix the counted loop in RetryFailed")
	// Unknown id: no-op, no panic.
	trace.AttachToolCallReasoning("call-404", "should go nowhere")
	// Empty reasoning: no-op.
	trace.AttachToolCallReasoning("call-1", "")

	done(captureResult{}, nil)

	if len(trace.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(trace.ToolCalls))
	}
	got, _ := trace.ToolCalls[0].Params["reasoning"].(string)
	if got != "Fix the counted loop in RetryFailed" {
		t.Errorf("params reasoning = %q, want the attached rationale", got)
	}
	if trace.ToolCalls[0].Params["path"] != "a.go" {
		t.Error("attach must not clobber existing params")
	}

	// A handler-recorded reasoning value must not be overwritten.
	tc2 := trace.StartToolCall("call-2", "write_file", map[string]any{"reasoning": "handler-recorded"})
	tc2.Complete(nil, nil)
	trace.AttachToolCallReasoning("call-2", "executor value")
	if got, _ := trace.ToolCalls[1].Params["reasoning"].(string); got != "handler-recorded" {
		t.Errorf("params reasoning = %q, want handler-recorded value preserved", got)
	}
}

// TestAppendReasoning_GatedOnPayloadOptIn confirms that raw model reasoning is
// only captured when the WithPayloadsEnabled opt-in is set on the trace
// context — thinking blocks are confidential completion content and must not
// be recorded by default. Empty content is always dropped.
func TestAppendReasoning_GatedOnPayloadOptIn(t *testing.T) {
	// Default context: payloads off — AppendReasoning is a no-op.
	ctx, _ := CaptureTrace[captureResult](t.Context())
	trace, done := StartTrace[captureResult](ctx, "prompt")
	trace.AppendReasoning(ReasoningContent{Thinking: "secret plan"})
	done(captureResult{}, nil)
	if len(trace.Reasoning) != 0 {
		t.Errorf("Reasoning = %d blocks, want 0 when payloads are disabled", len(trace.Reasoning))
	}

	// Opt-in context: payloads on — non-empty reasoning is captured, empty is dropped.
	onCtx, _ := CaptureTrace[captureResult](t.Context())
	onCtx = WithPayloadsEnabled(onCtx, true)
	traceOn, doneOn := StartTrace[captureResult](onCtx, "prompt")
	traceOn.AppendReasoning(ReasoningContent{Thinking: ""})
	traceOn.AppendReasoning(ReasoningContent{Thinking: "recorded plan"})
	doneOn(captureResult{}, nil)
	if len(traceOn.Reasoning) != 1 {
		t.Fatalf("Reasoning = %d blocks, want 1 when payloads are enabled", len(traceOn.Reasoning))
	}
	if got := traceOn.Reasoning[0].Thinking; got != "recorded plan" {
		t.Errorf("Reasoning[0].Thinking = %q, want %q", got, "recorded plan")
	}
}

// TestSummarizeTraceReasoning_PrefersToolRationales confirms mutating tools'
// per-call reasoning wins over thinking blocks, renders in call order, and
// dedupes repeated rationales; read-only tools are excluded.
func TestSummarizeTraceReasoning_PrefersToolRationales(t *testing.T) {
	ctx, _ := CaptureTrace[captureResult](t.Context())
	trace, done := StartTrace[captureResult](ctx, "prompt")
	trace.Reasoning = []ReasoningContent{{Thinking: "opening plan — must not render"}}

	for i, spec := range []struct{ id, name, reasoning string }{
		{"c1", "read_file", "explore the file"},           // read-only: excluded
		{"c2", "edit_file", "Fix counted loops in Run"},   // kept
		{"c3", "edit_file", "Fix counted loops in Run"},   // duplicate: deduped
		{"c4", "write_file", "Add example_test.go"},       // kept
		{"c5", "submit_result", "submitting the payload"}, // excluded
	} {
		tc := trace.StartToolCall(spec.id, spec.name, map[string]any{"i": i})
		tc.Complete(nil, nil)
		trace.AttachToolCallReasoning(spec.id, spec.reasoning)
	}
	done(captureResult{}, nil)

	got := SummarizeTraceReasoning(trace, 400)
	want := "- Fix counted loops in Run\n- Add example_test.go"
	if got != want {
		t.Errorf("SummarizeTraceReasoning() = %q, want %q", got, want)
	}
}

// TestSummarizeTraceReasoning_FallsBackToThinking confirms the extended-
// thinking blocks render when no mutating tool carried a rationale, and that
// a nil trace renders nothing.
func TestSummarizeTraceReasoning_FallsBackToThinking(t *testing.T) {
	ctx, _ := CaptureTrace[captureResult](t.Context())
	trace, done := StartTrace[captureResult](ctx, "prompt")
	trace.Reasoning = []ReasoningContent{{Thinking: "the only rationale available"}}
	tc := trace.StartToolCall("c1", "read_file", map[string]any{})
	tc.Complete(nil, nil)
	done(captureResult{}, nil)

	if got, want := SummarizeTraceReasoning(trace, 400), "- the only rationale available"; got != want {
		t.Errorf("SummarizeTraceReasoning() = %q, want %q", got, want)
	}
	if got := SummarizeTraceReasoning[captureResult](nil, 400); got != "" {
		t.Errorf("SummarizeTraceReasoning(nil) = %q, want empty", got)
	}
}
