/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package inmem

import (
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/conformance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWorkQueue(t *testing.T) {
	// Adjust this to a suitable period for testing things.
	// The conformance tests own adjusting MaximumBackoffPeriod.
	workqueue.BackoffPeriod = 1 * time.Second
	ctor := func(limit int) workqueue.Interface {
		return NewWorkQueue(limit)
	}

	conformance.TestSemantics(t, ctor)

	conformance.TestConcurrency(t, ctor)

	conformance.TestOwner(t, func(identity string) workqueue.Interface {
		return NewWorkQueue(1, WithIdentity(identity))
	})

	conformance.TestMaxRetry(t, ctor)

	conformance.TestBackoffDelay(t, ctor)
}

func Test_Get_KeyNotFound(t *testing.T) {
	wq := NewWorkQueue(10)
	ctx := t.Context()

	resp, err := wq.Get(ctx, "nonexistent-key")
	if err == nil {
		t.Fatalf("error: got = %v, want = non-nil", err)
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error type: got = %T, want = gRPC status error", err)
	}

	if got, want := st.Code(), codes.NotFound; got != want {
		t.Errorf("status code: got = %v, want = %v", got, want)
	}

	if resp != nil {
		t.Errorf("response: got = %v, want = nil", resp)
	}
}

func Test_Get_QueuedKey(t *testing.T) {
	wq := NewWorkQueue(10)
	ctx := t.Context()

	// Queue a key
	err := wq.Queue(ctx, "test-key", workqueue.Options{
		Priority: 5,
	})
	if err != nil {
		t.Fatalf("Queue failed: %v", err)
	}

	// Get the key
	resp, err := wq.Get(ctx, "test-key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if got, want := resp.Status, workqueue.KeyState_QUEUED; got != want {
		t.Errorf("status: got = %v, want = %v", got, want)
	}
	if got, want := resp.Key, "test-key"; got != want {
		t.Errorf("key: got = %v, want = %v", got, want)
	}
	if got, want := resp.Priority, int64(5); got != want {
		t.Errorf("priority: got = %v, want = %v", got, want)
	}
	if resp.QueuedTime == 0 {
		t.Errorf("queued time: got = %v, want = non-zero", resp.QueuedTime)
	}
}

func Test_Get_InProgressKey(t *testing.T) {
	wq := NewWorkQueue(10)
	ctx := t.Context()

	// Queue a key
	err := wq.Queue(ctx, "test-key", workqueue.Options{
		Priority: 3,
	})
	if err != nil {
		t.Fatalf("Queue failed: %v", err)
	}

	// Start processing the key
	_, qd, _, err := wq.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}

	if len(qd) == 0 {
		t.Fatalf("queued keys: got = %d, want = at least 1", len(qd))
	}

	_, err = qd[0].Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Get the key
	resp, err := wq.Get(ctx, "test-key")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if got, want := resp.Status, workqueue.KeyState_IN_PROGRESS; got != want {
		t.Errorf("status: got = %v, want = %v", got, want)
	}
	if got, want := resp.Key, "test-key"; got != want {
		t.Errorf("key: got = %v, want = %v", got, want)
	}
}
