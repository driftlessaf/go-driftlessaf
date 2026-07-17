/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
)

// blockingQueue is a workqueue whose Enumerate blocks until released,
// counting calls, so tests can hold dispatch passes in flight.
type blockingQueue struct {
	enumerations atomic.Int64
	release      chan struct{}
}

var _ workqueue.Interface = (*blockingQueue)(nil)

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

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Errorf("status: got = %d, want = %d", got, want)
	}
	if got := q.enumerations.Load(); got != 1 {
		t.Errorf("enumerations: got = %d, want = 1", got)
	}
}

func TestHandler_ShedsWithinPeriod(t *testing.T) {
	q := &blockingQueue{}
	h := Handler(q, 1, 10, func(context.Context, string, workqueue.Options) error { return nil }, 0,
		WithDispatchPeriod(time.Hour))

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
