/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"testing"
)

type captureResult struct{ Answer string }

// TestCaptureReasoning_ForwardsAndCaptures drives a trace with reasoning
// through the teed tracer exactly as an executor would (NewTrace via
// StartTrace, RecordTrace on completion) and asserts both that the inner
// tracer still observed the trace (CloudEvent/logging emission must not be
// silently dropped) and that the accessor returns the captured blocks.
func TestCaptureReasoning_ForwardsAndCaptures(t *testing.T) {
	var forwarded *Trace[captureResult]
	fake := ByCode[captureResult](func(tr *Trace[captureResult]) {
		forwarded = tr
	})

	ctx := WithTracer[captureResult](t.Context(), fake)
	ctx, blocks := CaptureReasoning[captureResult](ctx)

	if got := blocks(); got != nil {
		t.Errorf("blocks() before any trace completes = %v, want nil", got)
	}

	trace, done := StartTrace[captureResult](ctx, "prompt")
	trace.Reasoning = []ReasoningContent{{Thinking: "Looked at the issue and found the relevant package."}}
	done(captureResult{Answer: "ok"}, nil)

	if forwarded == nil {
		t.Fatal("inner tracer never received RecordTrace — CaptureReasoning must forward, not replace, the original tracer")
	}
	if forwarded != trace {
		t.Error("inner tracer received a different trace than the one completed")
	}
	got := blocks()
	if len(got) != 1 || got[0].Thinking != "Looked at the issue and found the relevant package." {
		t.Errorf("blocks() = %+v, want the trace's reasoning", got)
	}
}

// TestCaptureReasoning_NoReasoningStaysNil confirms a trace completing with
// no reasoning leaves the accessor nil, so SummarizeReasoning over the result
// renders nothing — required behavior for every run without extended
// thinking.
func TestCaptureReasoning_NoReasoningStaysNil(t *testing.T) {
	ctx, blocks := CaptureReasoning[captureResult](t.Context())

	_, done := StartTrace[captureResult](ctx, "prompt")
	done(captureResult{}, nil)

	if got := blocks(); got != nil {
		t.Errorf("blocks() = %+v, want nil when the trace carried no reasoning", got)
	}
}

// TestCaptureReasoning_LastNonEmptyWins confirms that when multiple traces
// complete under one capture (e.g. a retried agent run), the most recent
// trace with reasoning supersedes, and a later empty trace does not erase it.
func TestCaptureReasoning_LastNonEmptyWins(t *testing.T) {
	ctx, blocks := CaptureReasoning[captureResult](t.Context())

	t1, done1 := StartTrace[captureResult](ctx, "first")
	t1.Reasoning = []ReasoningContent{{Thinking: "first attempt"}}
	done1(captureResult{}, nil)

	t2, done2 := StartTrace[captureResult](ctx, "second")
	t2.Reasoning = []ReasoningContent{{Thinking: "second attempt"}}
	done2(captureResult{}, nil)

	_, done3 := StartTrace[captureResult](ctx, "third, no reasoning")
	done3(captureResult{}, nil)

	got := blocks()
	if len(got) != 1 || got[0].Thinking != "second attempt" {
		t.Errorf("blocks() = %+v, want the second attempt's reasoning", got)
	}
}

// TestCaptureReasoning_IndependentCaptures confirms two captures never share
// state.
func TestCaptureReasoning_IndependentCaptures(t *testing.T) {
	ctx1, blocks1 := CaptureReasoning[captureResult](t.Context())
	_, blocks2 := CaptureReasoning[captureResult](t.Context())

	tr, done := StartTrace[captureResult](ctx1, "prompt")
	tr.Reasoning = []ReasoningContent{{Thinking: "call one"}}
	done(captureResult{}, nil)

	if got := blocks1(); len(got) != 1 {
		t.Errorf("blocks1() = %+v, want one block", got)
	}
	if got := blocks2(); got != nil {
		t.Errorf("blocks2() = %+v, want nil — it must not observe call one's trace", got)
	}
}
