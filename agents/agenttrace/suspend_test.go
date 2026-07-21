/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Suspend must close the root invoke_agent span with status OK (NOT Error) and
// stamp the disposition + reason attributes, and it must leave Trace.Error nil
// so downstream graders treat the halt as a non-error terminal state.
func TestSuspendSetsOKStatusAndReason(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	const reason = "awaiting answer"
	trace.Suspend(reason)

	if !trace.Suspended {
		t.Error("Suspended: got = false, wanted = true")
	}
	if got := trace.SuspensionReason; got != reason {
		t.Errorf("SuspensionReason: got = %q, wanted = %q", got, reason)
	}
	if trace.Error != nil {
		t.Errorf("Error: got = %v, wanted = nil (suspension is not a failure)", trace.Error)
	}
	if trace.EndTime.IsZero() {
		t.Error("EndTime: got = zero, wanted = set")
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans: got = %d, wanted = 1", len(ended))
	}
	span := ended[0]
	if got, want := span.Status().Code, codes.Ok; got != want {
		t.Errorf("root span status: got = %v, wanted = %v (suspension must be OK, not Error)", got, want)
	}

	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if got := attrs[AttrDisposition]; got != DispositionSuspended {
		t.Errorf("%s: got = %q, wanted = %q", AttrDisposition, got, DispositionSuspended)
	}
	if got := attrs[AttrSuspensionReason]; got != reason {
		t.Errorf("%s: got = %q, wanted = %q", AttrSuspensionReason, got, reason)
	}
}

// The finalized guard makes complete and Suspend mutually exclusive: once a
// trace suspends, a later complete(err) must not re-close it as an error, and
// vice versa.
func TestSuspendCompleteAreIdempotent(t *testing.T) {
	t.Run("suspend wins over later complete", func(t *testing.T) {
		tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
		trace := tracer.NewTrace(t.Context(), randomString())

		trace.Suspend("halt")
		trace.complete("late result", errors.New("boom"))

		if !trace.Suspended {
			t.Error("Suspended: got = false, wanted = true")
		}
		if trace.Error != nil {
			t.Errorf("Error: got = %v, wanted = nil", trace.Error)
		}
	})

	t.Run("complete wins over later suspend", func(t *testing.T) {
		tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
		trace := tracer.NewTrace(t.Context(), randomString())

		trace.complete("done", nil)
		trace.Suspend("too late")

		if trace.Suspended {
			t.Error("Suspended: got = true, wanted = false")
		}
		if trace.SuspensionReason != "" {
			t.Errorf("SuspensionReason: got = %q, wanted = empty", trace.SuspensionReason)
		}
	})
}
