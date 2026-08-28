/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	oteltrace "go.opentelemetry.io/otel/trace"

	"chainguard.dev/driftlessaf/workqueue"
)

func randomSpanContext(t *testing.T) oteltrace.SpanContext {
	t.Helper()

	for {
		var traceID oteltrace.TraceID
		if _, err := cryptorand.Read(traceID[:]); err != nil {
			t.Fatalf("generate trace ID: %v", err)
		}
		var spanID oteltrace.SpanID
		if _, err := cryptorand.Read(spanID[:]); err != nil {
			t.Fatalf("generate span ID: %v", err)
		}
		if spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID: traceID,
			SpanID:  spanID,
		}); spanContext.IsValid() {
			return spanContext
		}
	}
}

func TestRequestTraceContext(t *testing.T) {
	headerSpanContext := randomSpanContext(t)
	activeSpanContext := randomSpanContext(t)
	tests := []struct {
		name string
		ctx  context.Context
		want oteltrace.SpanContext
	}{{
		name: "extracts header without active context",
		ctx:  t.Context(),
		want: headerSpanContext,
	}, {
		name: "preserves active context",
		ctx:  oteltrace.ContextWithSpanContext(t.Context(), activeSpanContext),
		want: activeSpanContext,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(tt.ctx, http.MethodPost, "/", nil)
			req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", headerSpanContext.TraceID(), headerSpanContext.SpanID()))
			got := oteltrace.SpanContextFromContext(requestTraceContext(req))
			if got.TraceID() != tt.want.TraceID() {
				t.Errorf("trace ID: got = %q, want = %q", got.TraceID(), tt.want.TraceID())
			}
			if got.SpanID() != tt.want.SpanID() {
				t.Errorf("span ID: got = %q, want = %q", got.SpanID(), tt.want.SpanID())
			}
		})
	}
}

// triggerCount reads the current value of the trigger counter for the given
// result, so tests can assert deltas against the process-global metric.
func triggerCount(t *testing.T, result string) float64 {
	t.Helper()
	c, err := mTriggers.GetMetricWithLabelValues(metricServiceName, metricRevisionName, result)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", result, err)
	}
	return testutil.ToFloat64(c)
}

// blockingQueue is a workqueue whose Enumerate blocks until released,
// counting calls, so tests can hold dispatch passes in flight.
type blockingQueue struct {
	enumerations atomic.Int64
	release      chan struct{}
}

var _ workqueue.Interface = (*blockingQueue)(nil)

func (b *blockingQueue) Identity() string { return "" }

func (b *blockingQueue) Queue(context.Context, string, workqueue.Options) error {
	return nil
}

func (b *blockingQueue) Enumerate(ctx context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	b.enumerations.Add(1)
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		}
	}
	return nil, nil, nil, nil
}

func (b *blockingQueue) Get(context.Context, string) (*workqueue.KeyState, error) {
	return &workqueue.KeyState{}, nil
}

func TestHandler_DispatchesEmptyQueue(t *testing.T) {
	q := &blockingQueue{}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Nanosecond))
	dispatched := triggerCount(t, triggerDispatched)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Errorf("status: got = %d, want = %d", got, want)
	}
	if got := q.enumerations.Load(); got != 1 {
		t.Errorf("enumerations: got = %d, want = 1", got)
	}
	if got, want := triggerCount(t, triggerDispatched)-dispatched, 1.0; got != want {
		t.Errorf("dispatched triggers: got = %v, want = %v", got, want)
	}
}

func TestHandler_ShedsWithinPeriod(t *testing.T) {
	q := &blockingQueue{}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Hour))
	dispatched := triggerCount(t, triggerDispatched)
	shed := triggerCount(t, triggerShed)

	// The first trigger consumes the period's admission.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
	if got := q.enumerations.Load(); got != 1 {
		t.Fatalf("enumerations: got = %d, want = 1", got)
	}

	// Further triggers within the period shed promptly with 200 and do not
	// dispatch.
	for i := range 3 {
		start := time.Now()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
		if got, want := rec.Code, http.StatusOK; got != want {
			t.Errorf("shed %d status: got = %d, want = %d", i, got, want)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("shed %d latency: got = %v, want prompt", i, elapsed)
		}
	}
	if got := q.enumerations.Load(); got != 1 {
		t.Errorf("enumerations after sheds: got = %d, want = 1", got)
	}
	if got, want := triggerCount(t, triggerDispatched)-dispatched, 1.0; got != want {
		t.Errorf("dispatched triggers: got = %v, want = %v", got, want)
	}
	if got, want := triggerCount(t, triggerShed)-shed, 3.0; got != want {
		t.Errorf("shed triggers: got = %v, want = %v", got, want)
	}
}

func TestHandler_PassesOverlap(t *testing.T) {
	const passes = 3
	q := &blockingQueue{release: make(chan struct{})}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Nanosecond))

	// Launch passes one at a time, each admitted while the prior ones are
	// still blocked inside Enumerate: sweeps must stack rather than being
	// bounded in flight.
	done := make(chan struct{}, passes)
	for i := range passes {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
			done <- struct{}{}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for q.enumerations.Load() < int64(i+1) {
			if time.Now().After(deadline) {
				t.Fatalf("enumerations: got = %d, want = %d before deadline", q.enumerations.Load(), i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}

	close(q.release)
	for range passes {
		<-done
	}
	if got, want := q.enumerations.Load(), int64(passes); got != want {
		t.Errorf("enumerations: got = %d, want = %d", got, want)
	}
}
