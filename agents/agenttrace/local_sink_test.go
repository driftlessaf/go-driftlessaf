/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"testing"
)

// TestLocalSpanSink_InvokedWithoutPerTracerEmitter is the core case for the CLI:
// the default tracer sets no per-tracer spanEmitter, yet the process-global sink
// must still receive every turn's span. This is the property that gives uniform,
// type-erased incremental capture across a multi-agent-type process.
func TestLocalSpanSink_InvokedWithoutPerTracerEmitter(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")
	// Note: trace.spanEmitter is left nil, as with NewDefaultTracer.

	var got []RecordedSpan
	restore := SetLocalSpanSink(func(_ context.Context, span RecordedSpan) error {
		got = append(got, span)
		return nil
	})
	defer restore()

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-5")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hello"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()

	if got, want := len(got), 1; got != want {
		t.Fatalf("local sink spans: got %d, want %d", got, want)
	}
	if want := trace.ID + "-t0"; got[0].SpanID != want {
		t.Errorf("span_id: got %q, want %q", got[0].SpanID, want)
	}
}

// TestLocalSpanSink_FiresAlongsidePerTracerEmitter proves the two paths are
// independent: a trace that has a per-tracer emitter (the CloudEvent path) and a
// process-global sink installed delivers the same span to both.
func TestLocalSpanSink_FiresAlongsidePerTracerEmitter(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")

	perTracer := 0
	trace.spanEmitter = func(context.Context, RecordedSpan) error { perTracer++; return nil }

	global := 0
	restore := SetLocalSpanSink(func(context.Context, RecordedSpan) error { global++; return nil })
	defer restore()

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-5")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hi"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()

	if perTracer != 1 || global != 1 {
		t.Fatalf("expected both emitters once: per-tracer=%d global=%d", perTracer, global)
	}
}

// TestLocalSpanSink_RestoreClears verifies the returned closure reinstalls the
// previous sink, so a run-scoped sink does not leak into the process global.
func TestLocalSpanSink_RestoreClears(t *testing.T) {
	restore := SetLocalSpanSink(func(context.Context, RecordedSpan) error { return nil })
	if currentLocalSpanSink() == nil {
		t.Fatal("sink should be installed after SetLocalSpanSink")
	}
	restore()
	if currentLocalSpanSink() != nil {
		t.Fatal("sink should be nil after restore")
	}
}

// TestLocalSpanSink_NoPayloadNoEmission confirms the sink is gated the same way
// the per-tracer emitter is: without WithPayloadsEnabled the turn records no
// request payload, buildRecordedSpan yields nothing, and the sink is not called.
func TestLocalSpanSink_NoPayloadNoEmission(t *testing.T) {
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(t.Context(), "prompt") // payloads NOT enabled

	called := false
	restore := SetLocalSpanSink(func(context.Context, RecordedSpan) error { called = true; return nil })
	defer restore()

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-5")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hi"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()

	if called {
		t.Error("sink must not fire when no payload was recorded (WithPayloadsEnabled off)")
	}
}
