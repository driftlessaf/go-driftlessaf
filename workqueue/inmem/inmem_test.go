/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package inmem

import (
	"errors"
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

	conformance.TestTenantFairness(t, func(limit int, tenant workqueue.TenantFunc) workqueue.Interface {
		return NewWorkQueue(limit, WithTenant(tenant))
	})
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

func TestWithoutTenantKeysAreNotTenantKeys(t *testing.T) {
	wq := NewWorkQueue(10)
	ctx := t.Context()
	for _, key := range []string{"a/1", "b/1"} {
		if err := wq.Queue(ctx, key, workqueue.Options{}); err != nil {
			t.Fatalf("Queue(%q) failed: %v", key, err)
		}
	}
	_, qd, _, err := wq.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}
	if _, err := qd[0].Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	wip, qd, _, err := wq.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}
	if _, ok := wip[0].(workqueue.TenantKey); ok {
		t.Errorf("in-progress key %q implements workqueue.TenantKey without WithTenant", wip[0].Name())
	}
	if _, ok := qd[0].(workqueue.TenantKey); ok {
		t.Errorf("queued key %q implements workqueue.TenantKey without WithTenant", qd[0].Name())
	}
}

func TestStartReportsLostClaim(t *testing.T) {
	wq := NewWorkQueue(10)
	ctx := t.Context()
	if err := wq.Queue(ctx, "key", workqueue.Options{}); err != nil {
		t.Fatalf("Queue failed: %v", err)
	}
	_, qd, _, err := wq.Enumerate(ctx)
	if err != nil {
		t.Fatalf("Enumerate failed: %v", err)
	}
	if _, err := qd[0].Start(ctx); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if _, err := qd[0].Start(ctx); !errors.Is(err, workqueue.ErrClaimLost) {
		t.Errorf("second Start: got = %v, want = an error wrapping workqueue.ErrClaimLost", err)
	}
}
