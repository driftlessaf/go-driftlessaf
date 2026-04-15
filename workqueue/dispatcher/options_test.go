/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/workqueue"
)

// captureEmitter is a test errorEmitter that records the last error context.
type captureEmitter struct {
	mu  sync.Mutex
	got *ErrorContext
}

func (c *captureEmitter) emit(_ context.Context, ec ErrorContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = &ec
}

func (c *captureEmitter) drain() {}

func (c *captureEmitter) result() *ErrorContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func withCapture(e *captureEmitter) Option {
	return func(c *config) { c.errors = e }
}

func TestErrorEmitter_Requeued(t *testing.T) {
	key := fmt.Sprintf("requeue-%d", rand.Int64())
	next := &mockKey{name: key}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("transient failure")
	}, 0, withCapture(cap))

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Key != key {
		t.Errorf("key: got = %q, wanted = %q", got.Key, key)
	}
	if got.Action != ErrorRequeued {
		t.Errorf("action: got = %v, wanted = %v", got.Action, ErrorRequeued)
	}
	if got.Err == nil || got.Err.Error() != "transient failure" {
		t.Errorf("err: got = %v, wanted = transient failure", got.Err)
	}
}

func TestErrorEmitter_DeadLettered(t *testing.T) {
	key := fmt.Sprintf("deadletter-%d", rand.Int64())
	next := &mockKey{name: key, attempts: 5}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("persistent failure")
	}, 5, withCapture(cap))

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Key != key {
		t.Errorf("key: got = %q, wanted = %q", got.Key, key)
	}
	if got.Action != ErrorDeadLettered {
		t.Errorf("action: got = %v, wanted = %v", got.Action, ErrorDeadLettered)
	}
	if got.Attempts != 5 {
		t.Errorf("attempts: got = %d, wanted = 5", got.Attempts)
	}
}

func TestErrorEmitter_Dropped(t *testing.T) {
	key := fmt.Sprintf("drop-%d", rand.Int64())
	next := &mockKey{name: key}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.NonRetriableError(errors.New("bad input"), "invalid key format")
	}, 0, withCapture(cap))

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Key != key {
		t.Errorf("key: got = %q, wanted = %q", got.Key, key)
	}
	if got.Action != ErrorDropped {
		t.Errorf("action: got = %v, wanted = %v", got.Action, ErrorDropped)
	}
	if got.NonRetriableReason != "invalid key format" {
		t.Errorf("reason: got = %q, wanted = %q", got.NonRetriableReason, "invalid key format")
	}
}

func TestErrorEmitter_NotCalledOnSuccess(t *testing.T) {
	next := &mockKey{name: "success"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, withCapture(cap))

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cap.result() != nil {
		t.Error("error emitter should not be called on success")
	}
}

func TestNopErrorEmitter(t *testing.T) {
	// Verify default nop emitter doesn't panic.
	next := &mockKey{name: "no-handler"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("fail")
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if next.requeue != 1 {
		t.Errorf("requeue: got = %d, wanted = 1", next.requeue)
	}
}

func TestErrorAction_String(t *testing.T) {
	tests := []struct {
		action ErrorAction
		want   string
	}{{
		action: ErrorRequeued,
		want:   "requeued",
	}, {
		action: ErrorDeadLettered,
		want:   "dead-lettered",
	}, {
		action: ErrorDropped,
		want:   "dropped",
	}, {
		action: ErrorAction(99),
		want:   "unknown",
	}}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.action.String(); got != tt.want {
				t.Errorf("String(): got = %q, wanted = %q", got, tt.want)
			}
		})
	}
}
