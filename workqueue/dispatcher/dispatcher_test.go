/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Mocks ---

type mockKey struct {
	name     string
	owner    string
	priority int64
	orphaned bool
	startErr error
	attempts int
	requeue  int
	dead     int
	// deadErr is what Deadletter returns after counting the call.
	deadErr  error
	complete int
	// bareRequeue counts calls to Requeue (no options). requeueOpts counts
	// calls to RequeueWithOptions and captures the last options passed, so a
	// test can assert which requeue path the dispatcher took.
	bareRequeue int
	requeueOpts int
	lastReqOpts workqueue.Options
	mu          sync.Mutex
}

// Implement Priority() to satisfy workqueue.QueuedKey.
func (m *mockKey) Priority() int64 {
	return m.priority
}

func (m *mockKey) Name() string     { return m.name }
func (m *mockKey) Owner() string    { return m.owner }
func (m *mockKey) IsOrphaned() bool { return m.orphaned }
func (m *mockKey) Start(context.Context) (workqueue.OwnedInProgressKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return nil, m.startErr
	}
	return &mockInProgressKey{mockKey: m}, nil
}

func (m *mockKey) Requeue(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requeue++
	m.bareRequeue++
	return nil
}

func (m *mockKey) RequeueWithOptions(_ context.Context, opts workqueue.Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requeue++
	m.requeueOpts++
	m.lastReqOpts = opts
	return nil
}

type mockInProgressKey struct {
	*mockKey
}

// Ensure mockInProgressKey implements workqueue.OwnedInProgressKey.
var _ workqueue.OwnedInProgressKey = (*mockInProgressKey)(nil)

func (m *mockInProgressKey) Context() context.Context { return context.Background() }
func (m *mockInProgressKey) Name() string             { return m.name }
func (m *mockInProgressKey) Priority() int64          { return 0 }
func (m *mockInProgressKey) GetAttempts() int         { return m.attempts }
func (m *mockInProgressKey) Complete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.complete++
	return nil
}

func (m *mockInProgressKey) Deadletter(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dead++
	return m.deadErr
}

type queuedItem struct {
	key  string
	opts workqueue.Options
}

type mockQueue struct {
	mu       sync.Mutex
	identity string
	wip      []workqueue.ObservedInProgressKey
	next     []workqueue.QueuedKey
	err      error
	queued   []queuedItem
	failKey  string // If set, Queue will fail for this key
}

type capacityAwareQueue struct {
	mockQueue
	capacity int
}

func (m *capacityAwareQueue) EnumerateWithCapacity(ctx context.Context, capacity int) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	m.capacity = capacity
	return m.Enumerate(ctx)
}

func (m *mockQueue) Identity() string { return m.identity }

func (m *mockQueue) Enumerate(context.Context) ([]workqueue.ObservedInProgressKey, []workqueue.QueuedKey, []workqueue.DeadLetteredKey, error) {
	return m.wip, m.next, nil, m.err
}

func (m *mockQueue) Queue(_ context.Context, key string, opts workqueue.Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failKey != "" && key == m.failKey {
		return errors.New("queue failed")
	}
	m.queued = append(m.queued, queuedItem{key: key, opts: opts})
	m.next = append(m.next, &mockKey{name: key})
	return nil
}

func (m *mockQueue) Get(_ context.Context, key string) (*workqueue.KeyState, error) {
	return nil, status.Errorf(codes.NotFound, "key %q not found", key)
}

func (m *mockQueue) getQueued() []queuedItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]queuedItem{}, m.queued...)
}

// --- Tests ---

func TestHandleAsync_EnumerateError(t *testing.T) {
	q := &mockQueue{err: errors.New("fail")}
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error { return nil }, 0)
	if err := future(); err == nil || err.Error() != "enumerate() = fail" {
		t.Errorf("expected enumerate error, got %v", err)
	}
}

func TestHandleAsync_UsesCapacityAwareEnumeration(t *testing.T) {
	q := &capacityAwareQueue{}
	future := HandleAsync(t.Context(), q, 7, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.capacity != 7 {
		t.Errorf("capacity passed to enumeration = %d, want 7", q.capacity)
	}
}

func TestHandleAsync_OrphanedWorkIsRequeued(t *testing.T) {
	orphan := &mockKey{name: "orphan", orphaned: true}
	q := &mockQueue{wip: []workqueue.ObservedInProgressKey{&mockInProgressKey{mockKey: orphan}}}
	called := false
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		called = true
		return nil
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orphan.requeue != 1 {
		t.Errorf("expected orphaned key to be requeued")
	}
	if called {
		t.Errorf("callback should not be called for orphaned key")
	}
}

// TestHandleAsync_OrphanOverMaxRetryIsDeadLettered pins the orphan sweep's
// budget check: an orphan whose attempt count already meets maxRetry is
// dead-lettered, not requeued, so a callback that kills its dispatcher before
// Deadletter can run does not re-claim the key forever. Below the budget, or
// with retries unlimited, the sweep requeues as before.
func TestHandleAsync_OrphanOverMaxRetryIsDeadLettered(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attempts    int
		maxRetry    int
		wantDead    int
		wantRequeue int
	}{
		{name: "at the budget dead-letters", attempts: 5, maxRetry: 5, wantDead: 1},
		{name: "over the budget dead-letters", attempts: 268, maxRetry: 5, wantDead: 1},
		{name: "under the budget requeues", attempts: 4, maxRetry: 5, wantRequeue: 1},
		{name: "unlimited retries requeue", attempts: 268, maxRetry: 0, wantRequeue: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orphan := &mockKey{name: "orphan", orphaned: true, attempts: tc.attempts}
			q := &mockQueue{wip: []workqueue.ObservedInProgressKey{&mockInProgressKey{mockKey: orphan}}}
			called := false
			future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
				called = true
				return nil
			}, tc.maxRetry)
			if err := future(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if orphan.dead != tc.wantDead || orphan.requeue != tc.wantRequeue {
				t.Errorf("dead = %d, requeue = %d; want dead = %d, requeue = %d", orphan.dead, orphan.requeue, tc.wantDead, tc.wantRequeue)
			}
			if called {
				t.Errorf("callback should not be called for orphaned key")
			}
		})
	}
}

// TestHandleAsync_OrphanDeadletterSkippedIsNotReported pins the sweep's
// handling of a dead-letter the backend declined: the lease changed between
// the listing and the move, so the key is still running under a live owner.
// The sweep neither fails, nor requeues, nor reports a dead-letter that did
// not happen.
func TestHandleAsync_OrphanDeadletterSkippedIsNotReported(t *testing.T) {
	orphan := &mockKey{
		name:     "orphan",
		orphaned: true,
		attempts: 5,
		deadErr:  fmt.Errorf("Deadletter(%q): %w", "orphan", workqueue.ErrDeadletterSkipped),
	}
	q := &mockQueue{wip: []workqueue.ObservedInProgressKey{&mockInProgressKey{mockKey: orphan}}}
	cap := &captureEmitter{}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		t.Error("callback should not be called for orphaned key")
		return nil
	}, 5, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orphan.dead != 1 || orphan.requeue != 0 {
		t.Errorf("dead = %d, requeue = %d; want dead = 1, requeue = 0", orphan.dead, orphan.requeue)
	}
	if got := cap.result(); got != nil {
		t.Errorf("error emitter called with action %v, want no call for a skipped dead-letter", got.Action)
	}
}

func TestHandleAsync_NoOpenSlots(t *testing.T) {
	active := &mockKey{name: "active"}
	q := &mockQueue{
		wip:  []workqueue.ObservedInProgressKey{active},
		next: []workqueue.QueuedKey{&mockKey{name: "next"}},
	}
	called := false
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		called = true
		return nil
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Errorf("callback should not be called when no open slots")
	}
}

func TestHandleAsync_LaunchesNewWork(t *testing.T) {
	next := &mockKey{name: "next"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	var called bool
	future := HandleAsync(context.Background(), q, 1, 0, func(_ context.Context, key string, _ workqueue.Options) error {
		called = true
		if key != "next" {
			t.Errorf("expected key 'next', got %q", key)
		}
		return nil
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Errorf("callback was not called")
	}
	if next.complete != 1 {
		t.Errorf("expected Complete to be called")
	}
}

func TestHandleAsync_CallbackFails_Requeue(t *testing.T) {
	next := &mockKey{name: "fail"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("fail")
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.requeue != 1 {
		t.Errorf("expected Requeue to be called")
	}
}

// TestHandleAsync_DefaultBackoff proves every retriable failure requeues on
// the default jittered doubling curve: never a bare requeue, BackoffDelay in
// [base, base+base/2), and no Delay (which would reset the attempt count).
func TestHandleAsync_DefaultBackoff(t *testing.T) {
	next := &mockKey{name: "fail", attempts: 1}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("fail")
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.bareRequeue != 0 {
		t.Errorf("bare Requeue: got = %d, want = 0", next.bareRequeue)
	}
	if next.requeueOpts != 1 {
		t.Fatalf("RequeueWithOptions calls: got = %d, want = 1", next.requeueOpts)
	}
	base := workqueue.BackoffPeriod
	if got := next.lastReqOpts.BackoffDelay; got < base || got >= base+base/2 {
		t.Errorf("BackoffDelay: got = %v, want in [%v, %v)", got, base, base+base/2)
	}
	if next.lastReqOpts.Delay != 0 {
		t.Errorf("Delay: got = %v, want = 0 (backoff must not reset attempts)", next.lastReqOpts.Delay)
	}
}

// TestHandleAsync_WithBackoff verifies the WithBackoff hook replaces the
// default curve when it returns a positive duration, and that a nil hook or
// non-positive return falls back to the default jittered doubling curve.
func TestHandleAsync_WithBackoff(t *testing.T) {
	curveMin := 2 * workqueue.BackoffPeriod // attempts=2 → base doubles once
	tests := []struct {
		name    string
		backoff func(attempts int) time.Duration
		// wantExact is the exact BackoffDelay when the hook drives it;
		// zero means "expect the default curve range for attempts=2".
		wantExact time.Duration
	}{{
		name:    "nil hook falls back to the default curve",
		backoff: nil,
	}, {
		name:    "non-positive return falls back to the default curve",
		backoff: func(int) time.Duration { return 0 },
	}, {
		name:      "positive return replaces the default curve",
		backoff:   func(attempts int) time.Duration { return time.Duration(attempts) * time.Second },
		wantExact: 2 * time.Second,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &mockKey{name: "fail", attempts: 2}
			q := &mockQueue{next: []workqueue.QueuedKey{next}}
			future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
				return errors.New("fail")
			}, 0, WithBackoff(tt.backoff))
			if err := future(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if next.bareRequeue != 0 {
				t.Errorf("bare Requeue: got = %d, want = 0", next.bareRequeue)
			}
			if next.requeueOpts != 1 {
				t.Fatalf("RequeueWithOptions calls: got = %d, want = 1", next.requeueOpts)
			}
			got := next.lastReqOpts.BackoffDelay
			if tt.wantExact != 0 {
				if got != tt.wantExact {
					t.Errorf("BackoffDelay: got = %v, want = %v", got, tt.wantExact)
				}
			} else if got < curveMin || got >= curveMin+curveMin/2 {
				t.Errorf("BackoffDelay: got = %v, want in [%v, %v)", got, curveMin, curveMin+curveMin/2)
			}
			// The backoff path must never reset the attempt count via Delay.
			if next.lastReqOpts.Delay != 0 {
				t.Errorf("Delay: got = %v, want = 0 (backoff must not reset attempts)", next.lastReqOpts.Delay)
			}
		})
	}
}

func TestHandleAsync_CallbackFails_DeadletterOnMaxRetry(t *testing.T) {
	next := &mockKey{name: "fail", attempts: 3}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	maxRetry := 3
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("fail")
	}, maxRetry)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 1 {
		t.Errorf("expected Deadletter to be called")
	}
}

func TestHandleAsync_CallbackFails_NonRetriable(t *testing.T) {
	next := &mockKey{name: "fail"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	nonRetriable := workqueue.NonRetriableError(errors.New("non-retriable"), "no retry")
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return nonRetriable
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.complete != 1 {
		t.Errorf("expected Complete to be called for non-retriable error")
	}
}

func TestHandleAsync_CallbackFails_ImmediateDeadLetter(t *testing.T) {
	next := &mockKey{name: "fail"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	dl := workqueue.DeadLetterError(errors.New("permanent refusal"), "permanent")
	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return dl
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 1 {
		t.Errorf("expected Deadletter to be called for a dead-letter error")
	}
	if next.complete != 0 {
		t.Errorf("expected Complete NOT to be called: a dead-letter error must never be silently dropped")
	}
	if next.requeue != 0 {
		t.Errorf("expected Requeue NOT to be called for a dead-letter error")
	}
}

// TestHandleAsync_OwnedDeadletterSkippedOnMaxRetryIsNotReported pins the
// owned-key max-retry branch when the backend declines the move: the key's
// pinned copy source is already gone, so the dispatcher neither fails, nor
// requeues, nor reports a dead-letter that did not happen.
func TestHandleAsync_OwnedDeadletterSkippedOnMaxRetryIsNotReported(t *testing.T) {
	next := &mockKey{
		name:     "fail",
		attempts: 3,
		deadErr:  fmt.Errorf("Deadletter(%q): %w", "fail", workqueue.ErrDeadletterSkipped),
	}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("fail")
	}, 3, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 1 || next.requeue != 0 {
		t.Errorf("dead = %d, requeue = %d; want dead = 1, requeue = 0", next.dead, next.requeue)
	}
	if got := cap.result(); got != nil {
		t.Errorf("error emitter called with action %v, want no call for a skipped dead-letter", got.Action)
	}
}

// TestHandleAsync_OwnedDeadletterSkippedOnDeadLetterErrorIsNotReported pins
// the same no-op handling on the callback's DeadLetterError branch.
func TestHandleAsync_OwnedDeadletterSkippedOnDeadLetterErrorIsNotReported(t *testing.T) {
	next := &mockKey{
		name:    "fail",
		deadErr: fmt.Errorf("Deadletter(%q): %w", "fail", workqueue.ErrDeadletterSkipped),
	}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}
	dl := workqueue.DeadLetterError(errors.New("permanent refusal"), "permanent")
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return dl
	}, 0, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 1 || next.requeue != 0 {
		t.Errorf("dead = %d, requeue = %d; want dead = 1, requeue = 0", next.dead, next.requeue)
	}
	if got := cap.result(); got != nil {
		t.Errorf("error emitter called with action %v, want no call for a skipped dead-letter", got.Action)
	}
}

func TestHandleAsync_RespectsBatchSize(t *testing.T) {
	keys := []*mockKey{{
		name: "k1",
	}, {
		name: "k2",
	}, {
		name: "k3",
	}}

	next := make([]workqueue.QueuedKey, len(keys))
	for i := range keys {
		next[i] = keys[i]
	}

	q := &mockQueue{next: next}

	future := HandleAsync(context.Background(), q, 3, 2, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var launched int
	for _, k := range keys {
		launched += k.complete
	}

	if launched != 2 {
		t.Fatalf("expected to launch 2 keys, got %d", launched)
	}
}

func TestHandleAsync_RespectsOwnerConcurrency(t *testing.T) {
	const (
		identity       = "us-central1"
		remoteIdentity = "us-east1"
	)
	keys := []*mockKey{{name: "k1"}, {name: "k2"}, {name: "k3"}}
	next := make([]workqueue.QueuedKey, len(keys))
	for i := range keys {
		next[i] = keys[i]
	}

	q := &mockQueue{
		identity: identity,
		wip: []workqueue.ObservedInProgressKey{
			&mockInProgressKey{mockKey: &mockKey{name: "local", owner: identity}},
			&mockInProgressKey{mockKey: &mockKey{name: "remote-1", owner: remoteIdentity}},
			&mockInProgressKey{mockKey: &mockKey{name: "remote-2", owner: remoteIdentity}},
		},
		next: next,
	}

	future := HandleAsync(context.Background(), q, 5, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, WithOwnerConcurrency(2))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	launched := 0
	for _, k := range keys {
		launched += k.complete
	}
	if launched != 1 {
		t.Fatalf("expected to launch 1 key, got %d", launched)
	}
}

func TestHandleAsync_OwnerConcurrencyAtLimit(t *testing.T) {
	const identity = "us-central1"
	next := &mockKey{name: "next"}
	q := &mockQueue{
		identity: identity,
		wip: []workqueue.ObservedInProgressKey{
			&mockInProgressKey{mockKey: &mockKey{name: "local", owner: identity}},
		},
		next: []workqueue.QueuedKey{next},
	}

	future := HandleAsync(context.Background(), q, 2, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, WithOwnerConcurrency(1))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.complete != 0 {
		t.Fatalf("expected no keys to launch, got %d", next.complete)
	}
}

func TestHandleAsync_OwnerConcurrencyRequiresIdentity(t *testing.T) {
	future := HandleAsync(context.Background(), &mockQueue{}, 1, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, WithOwnerConcurrency(1))
	if err := future(); err == nil {
		t.Fatal("expected an error for an empty identity")
	}
}

// TestHandleAsync_RequeueSucceedsWithCanceledContext tests that cleanup operations
// (Requeue, Complete, Deadletter) succeed even when the parent context is canceled.
// This is critical for graceful shutdown - when Cloud Run sends SIGTERM, we need to
// ensure work items are properly requeued rather than left stuck in "in-progress" state.
func TestHandleAsync_RequeueSucceedsWithCanceledContext(t *testing.T) {
	next := &mockKey{name: "will-fail"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	// Create a context that we'll cancel during the callback
	ctx, cancel := context.WithCancel(context.Background())

	future := HandleAsync(ctx, q, 1, 0, func(context.Context, string, workqueue.Options) error {
		// Simulate SIGTERM arriving during work - cancel the context
		cancel()
		// Return an error to trigger requeue
		return errors.New("work interrupted")
	}, 0)

	// The future should complete without error (dispatcher shouldn't fail)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Critical: Requeue should have been called despite context cancellation
	if next.requeue != 1 {
		t.Errorf("expected Requeue to be called even with canceled context, got requeue=%d", next.requeue)
	}
}

// TestHandleAsync_CompleteSucceedsWithCanceledContext tests that Complete succeeds
// even when the parent context is canceled during successful work completion.
func TestHandleAsync_CompleteSucceedsWithCanceledContext(t *testing.T) {
	next := &mockKey{name: "will-succeed"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	ctx, cancel := context.WithCancel(context.Background())

	future := HandleAsync(ctx, q, 1, 0, func(context.Context, string, workqueue.Options) error {
		// Simulate context cancellation happening right before completion
		cancel()
		return nil // Success - should trigger Complete
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Critical: Complete should have been called despite context cancellation
	if next.complete != 1 {
		t.Errorf("expected Complete to be called even with canceled context, got complete=%d", next.complete)
	}
}

// TestHandleAsync_OrphanRequeueSucceedsWithCanceledContext tests that orphaned work
// requeue succeeds even when the context is canceled.
func TestHandleAsync_OrphanRequeueSucceedsWithCanceledContext(t *testing.T) {
	orphan := &mockKey{name: "orphan", orphaned: true}
	q := &mockQueue{wip: []workqueue.ObservedInProgressKey{&mockInProgressKey{mockKey: orphan}}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately
	cancel()

	future := HandleAsync(ctx, q, 1, 0, func(context.Context, string, workqueue.Options) error {
		t.Error("callback should not be called for orphaned key")
		return nil
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Critical: Orphan requeue should succeed despite canceled context
	if orphan.requeue != 1 {
		t.Errorf("expected orphaned key requeue even with canceled context, got requeue=%d", orphan.requeue)
	}
}

// --- Queue Keys from Response Tests ---

// TestHandleAsync_QueueKeysBasic tests that returning QueueKeys from the callback
// results in those keys being queued before the current key is completed.
func TestHandleAsync_QueueKeysBasic(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: "child1"},
			workqueue.QueueKey{Key: "child2"},
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parent should be completed
	if next.complete != 1 {
		t.Errorf("expected Complete to be called, got complete=%d", next.complete)
	}

	// Children should be queued
	queued := q.getQueued()
	if len(queued) != 2 {
		t.Fatalf("expected 2 keys to be queued, got %d", len(queued))
	}
	wantKeys := []string{"child1", "child2"}
	for i, want := range wantKeys {
		if queued[i].key != want {
			t.Errorf("queued[%d].key: got = %q, wanted = %q", i, queued[i].key, want)
		}
	}
}

// TestHandleAsync_QueueKeysWithPriority tests that keys queued via QueueKeys
// respect priority settings.
func TestHandleAsync_QueueKeysWithPriority(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: "high", Priority: 100},
			workqueue.QueueKey{Key: "low", Priority: 10},
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	queued := q.getQueued()
	if len(queued) != 2 {
		t.Fatalf("expected 2 keys to be queued, got %d", len(queued))
	}

	// Verify priorities were passed correctly
	if queued[0].opts.Priority != 100 {
		t.Errorf("queued[0].opts.Priority: got = %d, wanted = 100", queued[0].opts.Priority)
	}
	if queued[1].opts.Priority != 10 {
		t.Errorf("queued[1].opts.Priority: got = %d, wanted = 10", queued[1].opts.Priority)
	}
}

// TestHandleAsync_QueueKeysWithDelay tests that keys queued via QueueKeys
// respect delay settings (NotBefore).
func TestHandleAsync_QueueKeysWithDelay(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	delaySeconds := int64(60)
	before := time.Now()

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: "delayed", DelaySeconds: delaySeconds},
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	after := time.Now()

	queued := q.getQueued()
	if len(queued) != 1 {
		t.Fatalf("expected 1 key to be queued, got %d", len(queued))
	}

	// NotBefore should be approximately now + delaySeconds
	expectedNotBefore := before.Add(time.Duration(delaySeconds) * time.Second)
	maxNotBefore := after.Add(time.Duration(delaySeconds) * time.Second)

	if queued[0].opts.NotBefore.Before(expectedNotBefore) {
		t.Errorf("NotBefore too early: got = %v, wanted >= %v", queued[0].opts.NotBefore, expectedNotBefore)
	}
	if queued[0].opts.NotBefore.After(maxNotBefore) {
		t.Errorf("NotBefore too late: got = %v, wanted <= %v", queued[0].opts.NotBefore, maxNotBefore)
	}
}

// TestHandleAsync_QueueKeysOnFailure tests that queue_keys are NOT processed
// when the callback returns a real error (not just a QueueKeys sentinel).
func TestHandleAsync_QueueKeysOnFailure(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		// Return a real error - queue_keys should NOT be processed
		return errors.New("processing failed")
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parent should be requeued (failed)
	if next.requeue != 1 {
		t.Errorf("expected Requeue to be called, got requeue=%d", next.requeue)
	}
	if next.complete != 0 {
		t.Errorf("expected Complete NOT to be called, got complete=%d", next.complete)
	}

	// No children should be queued
	queued := q.getQueued()
	if len(queued) != 0 {
		t.Errorf("expected 0 keys to be queued on failure, got %d", len(queued))
	}
}

// TestHandleAsync_QueueKeysSelf tests that including the current key in QueueKeys
// results in it being requeued (enters "dual state").
func TestHandleAsync_QueueKeysSelf(t *testing.T) {
	next := &mockKey{name: "self-requeue"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(_ context.Context, key string, _ workqueue.Options) error {
		// Requeue self via QueueKeys
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: key, DelaySeconds: 30},
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Current key should be completed (in-progress work done)
	if next.complete != 1 {
		t.Errorf("expected Complete to be called, got complete=%d", next.complete)
	}

	// Self should be queued again
	queued := q.getQueued()
	if len(queued) != 1 {
		t.Fatalf("expected 1 key to be queued, got %d", len(queued))
	}
	if queued[0].key != "self-requeue" {
		t.Errorf("queued key: got = %q, wanted = %q", queued[0].key, "self-requeue")
	}
}

// TestHandleAsync_QueueKeysAndChildren tests combining self-requeue with child keys.
func TestHandleAsync_QueueKeysAndChildren(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(_ context.Context, key string, _ workqueue.Options) error {
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: "child1"},
			workqueue.QueueKey{Key: "child2", Priority: 50},
			workqueue.QueueKey{Key: key, DelaySeconds: 120}, // Requeue self
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Current key should be completed
	if next.complete != 1 {
		t.Errorf("expected Complete to be called, got complete=%d", next.complete)
	}

	// All three keys should be queued
	queued := q.getQueued()
	if len(queued) != 3 {
		t.Fatalf("expected 3 keys to be queued, got %d", len(queued))
	}

	wantKeys := []string{"child1", "child2", "parent"}
	for i, want := range wantKeys {
		if queued[i].key != want {
			t.Errorf("queued[%d].key: got = %q, wanted = %q", i, queued[i].key, want)
		}
	}
}

// TestHandleAsync_QueueKeysFailure tests that if queueing a key fails,
// the current key is requeued (not completed).
func TestHandleAsync_QueueKeysFailure(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{
		next:    []workqueue.QueuedKey{next},
		failKey: "fail-me", // Queue will fail for this key
	}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.QueueKeys(
			workqueue.QueueKey{Key: "child1"},
			workqueue.QueueKey{Key: "fail-me"}, // This will fail
			workqueue.QueueKey{Key: "child2"},  // This won't be reached
		)
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parent should be requeued (queue operation failed)
	if next.requeue != 1 {
		t.Errorf("expected Requeue to be called, got requeue=%d", next.requeue)
	}
	if next.complete != 0 {
		t.Errorf("expected Complete NOT to be called, got complete=%d", next.complete)
	}

	// Only child1 should have been queued (before the failure)
	queued := q.getQueued()
	if len(queued) != 1 {
		t.Fatalf("expected 1 key to be queued before failure, got %d", len(queued))
	}
	if queued[0].key != "child1" {
		t.Errorf("queued key: got = %q, wanted = %q", queued[0].key, "child1")
	}
}

// TestHandleAsync_EmptyQueueKeys tests that returning QueueKeys with no keys
// (returns nil) results in normal completion.
func TestHandleAsync_EmptyQueueKeys(t *testing.T) {
	next := &mockKey{name: "parent"}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}

	future := HandleAsync(context.Background(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		// Empty QueueKeys returns nil
		return workqueue.QueueKeys()
	}, 0)

	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Parent should be completed
	if next.complete != 1 {
		t.Errorf("expected Complete to be called, got complete=%d", next.complete)
	}

	// No keys should be queued
	queued := q.getQueued()
	if len(queued) != 0 {
		t.Errorf("expected 0 keys to be queued, got %d", len(queued))
	}
}

// TestServiceCallback_QueueKeys tests that ServiceCallback correctly translates
// ProcessResponse queue_keys to QueueKeys sentinel error.
func TestServiceCallback_QueueKeys(t *testing.T) {
	tests := []struct {
		name     string
		resp     *workqueue.ProcessResponse
		wantKeys []workqueue.QueueKey
	}{{
		name:     "no queue keys",
		resp:     &workqueue.ProcessResponse{},
		wantKeys: nil,
	}, {
		name: "single queue key",
		resp: &workqueue.ProcessResponse{
			QueueKeys: []*workqueue.QueueKeyRequest{{
				Key: "child1",
			}},
		},
		wantKeys: []workqueue.QueueKey{{Key: "child1"}},
	}, {
		name: "multiple queue keys with options",
		resp: &workqueue.ProcessResponse{
			QueueKeys: []*workqueue.QueueKeyRequest{{
				Key:      "high-priority",
				Priority: 100,
			}, {
				Key:          "delayed",
				DelaySeconds: 60,
			}},
		},
		wantKeys: []workqueue.QueueKey{{
			Key:      "high-priority",
			Priority: 100,
		}, {
			Key:          "delayed",
			DelaySeconds: 60,
		}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock client that returns the test response
			client := &mockWorkqueueServiceClient{resp: tt.resp}
			cb := ServiceCallback(client)

			err := cb(context.Background(), "test-key", workqueue.Options{})
			gotKeys := workqueue.GetQueueKeys(err)

			if diff := cmp.Diff(tt.wantKeys, gotKeys); diff != "" {
				t.Errorf("GetQueueKeys() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestServiceCallback_RequeueAfter tests that ServiceCallback correctly translates
// ProcessResponse requeue_after_seconds to RequeueAfter sentinel error.
func TestServiceCallback_RequeueAfter(t *testing.T) {
	client := &mockWorkqueueServiceClient{
		resp: &workqueue.ProcessResponse{
			RequeueAfterSeconds: 30,
		},
	}
	cb := ServiceCallback(client)

	err := cb(context.Background(), "test-key", workqueue.Options{})

	// Should be a RequeueAfter error, not QueueKeys
	delay, ok := workqueue.GetRequeueDelay(err)
	if !ok {
		t.Fatal("expected RequeueAfter error")
	}
	if delay != 30*time.Second {
		t.Errorf("delay: got = %v, wanted = 30s", delay)
	}

	// Should NOT have queue keys
	if keys := workqueue.GetQueueKeys(err); keys != nil {
		t.Errorf("expected no queue keys, got %v", keys)
	}
}

// mockWorkqueueServiceClient implements WorkqueueServiceClient for testing.
type mockWorkqueueServiceClient struct {
	workqueue.WorkqueueServiceClient
	resp *workqueue.ProcessResponse
	err  error
}

func (m *mockWorkqueueServiceClient) Process(_ context.Context, _ *workqueue.ProcessRequest, _ ...grpc.CallOption) (*workqueue.ProcessResponse, error) {
	return m.resp, m.err
}

// TestRetryBackoff pins the failure-retry backoff curve: the base period
// doubling per recorded attempt, capped at the maximum, with malformed
// (sub-one) attempt counts yielding the base period.
func TestRetryBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 30 * time.Second},
		{attempts: 1, want: 30 * time.Second},
		{attempts: 2, want: time.Minute},
		{attempts: 3, want: 2 * time.Minute},
		{attempts: 4, want: 4 * time.Minute},
		{attempts: 5, want: 8 * time.Minute},
		{attempts: 6, want: 10 * time.Minute},
		{attempts: 100, want: 10 * time.Minute},
	}
	for _, tt := range tests {
		if got := retryBackoff(tt.attempts); got != tt.want {
			t.Errorf("retryBackoff(%d): got = %v, want = %v", tt.attempts, got, tt.want)
		}
	}
}

// TestHandleAsync_InfraFailure_BackoffRequeue verifies that an
// infrastructure-classified failure requeues on the same default curve as
// any other failure, without resetting the attempt count, and is emitted
// with Infrastructure=true.
func TestHandleAsync_InfraFailure_BackoffRequeue(t *testing.T) {
	next := &mockKey{name: "infra", attempts: 2}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return status.Error(codes.Unavailable, "upstream connect error or disconnect/reset before headers. reset reason: connection termination")
	}, 0, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.bareRequeue != 0 {
		t.Errorf("bare Requeue: got = %d, want = 0", next.bareRequeue)
	}
	if next.requeueOpts != 1 {
		t.Fatalf("RequeueWithOptions calls: got = %d, want = 1", next.requeueOpts)
	}
	// attempts=2 yields one doubling of the base with up to 50% additive jitter.
	base := 2 * workqueue.BackoffPeriod
	if got := next.lastReqOpts.BackoffDelay; got < base || got >= base+base/2 {
		t.Errorf("BackoffDelay: got = %v, want in [%v, %v)", got, base, base+base/2)
	}
	// The infra path must never reset the attempt count via Delay.
	if next.lastReqOpts.Delay != 0 {
		t.Errorf("Delay: got = %v, want = 0 (infra backoff must not reset attempts)", next.lastReqOpts.Delay)
	}
	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Action != ErrorRequeued {
		t.Errorf("action: got = %v, want = %v", got.Action, ErrorRequeued)
	}
	if !got.Infrastructure {
		t.Error("infrastructure: got = false, want = true")
	}
}

// TestHandleAsync_BackoffHook_AppliesToInfraFailures verifies the WithBackoff
// hook drives the delay for infrastructure failures too: scheduling treats
// every retriable failure alike, so there is no separate curve to bypass.
func TestHandleAsync_BackoffHook_AppliesToInfraFailures(t *testing.T) {
	next := &mockKey{name: "infra", attempts: 1}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return status.Error(codes.Unavailable, "no healthy upstream")
	}, 0, WithBackoff(func(int) time.Duration { return time.Second }))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.requeueOpts != 1 {
		t.Fatalf("RequeueWithOptions calls: got = %d, want = 1", next.requeueOpts)
	}
	if got := next.lastReqOpts.BackoffDelay; got != time.Second {
		t.Errorf("BackoffDelay: got = %v, want = 1s (hook drives all failures)", got)
	}
}

// TestHandleAsync_InfraFailure_DeadletterStillWins verifies the dead-letter
// cutoff takes precedence over the infrastructure backoff: attempts still
// count, so a key that only ever fails for infrastructure reasons (e.g. an
// input that OOM-kills its receiver) is dead-lettered at the limit, and the
// emitted context still records the infrastructure classification.
func TestHandleAsync_InfraFailure_DeadletterStillWins(t *testing.T) {
	next := &mockKey{name: "infra", attempts: 3}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return status.Error(codes.Unavailable, "connection termination")
	}, 3, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 1 {
		t.Errorf("Deadletter calls: got = %d, want = 1", next.dead)
	}
	if next.requeue != 0 {
		t.Errorf("Requeue calls: got = %d, want = 0", next.requeue)
	}
	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Action != ErrorDeadLettered {
		t.Errorf("action: got = %v, want = %v", got.Action, ErrorDeadLettered)
	}
	if !got.Infrastructure {
		t.Error("infrastructure: got = false, want = true")
	}
}

// TestHandleAsync_InfraFailure_NonRetriableWins verifies an explicit
// non-retriable marking takes precedence over the infrastructure
// classification: the reconciler's verdict to drop the key is respected.
func TestHandleAsync_InfraFailure_NonRetriableWins(t *testing.T) {
	next := &mockKey{name: "infra", attempts: 1}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return workqueue.NonRetriableError(status.Error(codes.Unavailable, "gone"), "do not retry")
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.complete != 1 {
		t.Errorf("Complete calls: got = %d, want = 1", next.complete)
	}
	if next.requeue != 0 {
		t.Errorf("Requeue calls: got = %d, want = 0", next.requeue)
	}
}

// TestHandleAsync_AppFailure_NotInfrastructure verifies an ordinary
// application failure requeues on the default curve and is emitted with
// Infrastructure=false.
func TestHandleAsync_AppFailure_NotInfrastructure(t *testing.T) {
	next := &mockKey{name: "app", attempts: 1}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	cap := &captureEmitter{}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return errors.New("reconcile failed")
	}, 0, withCapture(cap))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.requeueOpts != 1 {
		t.Errorf("RequeueWithOptions calls: got = %d, want = 1", next.requeueOpts)
	}
	if got, base := next.lastReqOpts.BackoffDelay, workqueue.BackoffPeriod; got < base || got >= base+base/2 {
		t.Errorf("BackoffDelay: got = %v, want in [%v, %v)", got, base, base+base/2)
	}
	got := cap.result()
	if got == nil {
		t.Fatal("error emitter was not called")
	}
	if got.Infrastructure {
		t.Error("infrastructure: got = true, want = false")
	}
}

// TestHandleAsync_InterruptedByShutdown_DoesNotConsumeAttempt proves a callback
// that fails after the dispatch context ended (the dispatcher is shutting down)
// is requeued with attempt-resetting Delay semantics and never dead-lettered,
// even at the retry limit: the interruption is infrastructure's doing.
func TestHandleAsync_InterruptedByShutdown_DoesNotConsumeAttempt(t *testing.T) {
	next := &mockKey{name: "interrupted", attempts: 5}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	future := HandleAsync(ctx, q, 1, 0, func(context.Context, string, workqueue.Options) error {
		cancel() // SIGTERM lands while the callback is in flight.
		return context.Canceled
	}, 5)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.dead != 0 {
		t.Errorf("dead-lettered %d times, want 0: an interrupted attempt must not consume the retry budget", next.dead)
	}
	if next.requeueOpts != 1 {
		t.Fatalf("RequeueWithOptions calls = %d, want 1", next.requeueOpts)
	}
	if next.lastReqOpts.Delay <= 0 || next.lastReqOpts.BackoffDelay != 0 {
		t.Errorf("requeue options = %+v, want a Delay (attempt reset) and no BackoffDelay", next.lastReqOpts)
	}
}

// TestHandleAsync_FailureWithLiveContext_ConsumesAttempt pins the boundary of
// the interruption path: the same error under a live dispatch context is the
// work's own failure and keeps the attempt-preserving backoff.
func TestHandleAsync_FailureWithLiveContext_ConsumesAttempt(t *testing.T) {
	next := &mockKey{name: "failed", attempts: 2}
	q := &mockQueue{next: []workqueue.QueuedKey{next}}
	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return context.Canceled
	}, 5)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if next.requeueOpts != 1 {
		t.Fatalf("RequeueWithOptions calls = %d, want 1", next.requeueOpts)
	}
	if next.lastReqOpts.BackoffDelay <= 0 || next.lastReqOpts.Delay != 0 {
		t.Errorf("requeue options = %+v, want a BackoffDelay (attempt kept) and no Delay", next.lastReqOpts)
	}
}

// --- Candidate ordering ---

// queuedKeys builds n equal-priority candidates named k0..k(n-1), in the order
// a queue would enumerate them.
func queuedKeys(n int) []workqueue.QueuedKey {
	out := make([]workqueue.QueuedKey, n)
	for i := range out {
		out[i] = &mockKey{name: fmt.Sprintf("k%d", i)}
	}
	return out
}

func keyNames(keys []workqueue.QueuedKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Name()
	}
	return out
}

func seededShuffle(seed uint64) func(n int, swap func(i, j int)) {
	return rand.New(rand.NewPCG(seed, 0)).Shuffle
}

func TestOrderCandidates_PreservesPriority(t *testing.T) {
	next := []workqueue.QueuedKey{
		&mockKey{name: "hi-1", priority: 10},
		&mockKey{name: "hi-2", priority: 10},
		&mockKey{name: "hi-3", priority: 10},
		&mockKey{name: "mid-1", priority: 5},
		&mockKey{name: "mid-2", priority: 5},
		&mockKey{name: "lo-1", priority: 0},
	}

	for seed := range uint64(50) {
		got := orderCandidates(next, nil, 32, seededShuffle(seed))
		if len(got) != len(next) {
			t.Fatalf("seed %d: got %d keys, want %d", seed, len(got), len(next))
		}
		byPriority := map[int64][]string{}
		for i, k := range got {
			if i > 0 && got[i-1].Priority() < k.Priority() {
				t.Fatalf("seed %d: priority %d at %d precedes higher priority %d: %v",
					seed, got[i-1].Priority(), i-1, k.Priority(), keyNames(got))
			}
			byPriority[k.Priority()] = append(byPriority[k.Priority()], k.Name())
		}
		for priority, want := range map[int64]int{10: 3, 5: 2, 0: 1} {
			if len(byPriority[priority]) != want {
				t.Fatalf("seed %d: priority %d has %d keys, want %d", seed, priority, len(byPriority[priority]), want)
			}
		}
	}
}

func TestOrderCandidates_WindowBound(t *testing.T) {
	next := queuedKeys(10)
	inWindow := map[string]struct{}{"k0": {}, "k1": {}, "k2": {}, "k3": {}}

	moved := false
	for seed := range uint64(50) {
		got := keyNames(orderCandidates(next, nil, 4, seededShuffle(seed)))
		for _, name := range got[:4] {
			if _, ok := inWindow[name]; !ok {
				t.Fatalf("seed %d: %q entered the window from outside it: %v", seed, name, got)
			}
		}
		if diff := cmp.Diff([]string{"k4", "k5", "k6", "k7", "k8", "k9"}, got[4:]); diff != "" {
			t.Fatalf("seed %d: tail reordered (-want +got):\n%s", seed, diff)
		}
		if got[0] != "k0" {
			moved = true
		}
	}
	if !moved {
		t.Fatal("no seed moved the head, the window is not being shuffled")
	}
}

func TestOrderCandidates_DropsActive(t *testing.T) {
	next := queuedKeys(4)
	active := map[string]struct{}{"k0": {}}

	for seed := range uint64(50) {
		got := keyNames(orderCandidates(next, active, 2, seededShuffle(seed)))
		if diff := cmp.Diff(3, len(got)); diff != "" {
			t.Fatalf("seed %d: wrong candidate count (-want +got):\n%s", seed, diff)
		}
		for _, name := range got {
			if name == "k0" {
				t.Fatalf("seed %d: active key k0 was returned: %v", seed, got)
			}
		}
		// The dropped key must not spend a window position: the window of two
		// covers the next two eligible keys, not k1 alone.
		window := map[string]struct{}{got[0]: {}, got[1]: {}}
		_, hasK1 := window["k1"]
		_, hasK2 := window["k2"]
		if !hasK1 || !hasK2 {
			t.Fatalf("seed %d: window %v does not cover k1 and k2", seed, got[:2])
		}
		if got[2] != "k3" {
			t.Fatalf("seed %d: tail is %q, want k3", seed, got[2])
		}
	}
}

func TestOrderCandidates_DisabledPassesInputThrough(t *testing.T) {
	next := queuedKeys(4)
	active := map[string]struct{}{"k0": {}}

	for _, tc := range []struct {
		name    string
		window  int
		shuffle func(n int, swap func(i, j int))
	}{
		{name: "nil_shuffle", window: DefaultCandidateWindowFactor, shuffle: nil},
		{name: "zero_window", window: 0, shuffle: seededShuffle(1)},
		{name: "negative_window", window: -1, shuffle: seededShuffle(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Active keys stay in place here: the dispatch loop skips them
			// itself, so a disabled ordering has nothing to drop.
			got := keyNames(orderCandidates(next, active, tc.window, tc.shuffle))
			if diff := cmp.Diff([]string{"k0", "k1", "k2", "k3"}, got); diff != "" {
				t.Errorf("candidates were reordered or filtered (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOrderCandidates_SeededDeterministic(t *testing.T) {
	next := queuedKeys(32)

	first := keyNames(orderCandidates(next, nil, 32, seededShuffle(7)))
	again := keyNames(orderCandidates(next, nil, 32, seededShuffle(7)))
	if diff := cmp.Diff(first, again); diff != "" {
		t.Fatalf("same seed produced different orders (-first +again):\n%s", diff)
	}

	other := keyNames(orderCandidates(next, nil, 32, seededShuffle(8)))
	if cmp.Diff(first, other) == "" {
		t.Fatal("seeds 7 and 8 produced the same order over 32 keys")
	}
}

// contentionRates simulates one dispatch pass per owner over the same queue:
// each owner orders the candidates independently and claims its first
// launchLimit keys. Returns the share of claim attempts lost to collisions and
// the per-owner share of successful claims when the first owner always wins a
// contested key.
// contentionOwners and contentionTrials size the contention model: three
// dispatchers is the deployed topology, and the trial count is what makes the
// measured rates stable to a tenth of a point.
const (
	contentionOwners = 3
	contentionTrials = 2000
)

func contentionRates(t *testing.T, launchLimit, window int, spread bool) (float64, []float64) {
	t.Helper()
	owners, trials := contentionOwners, contentionTrials
	next := queuedKeys(200)

	attempts, lost := 0, 0
	wins := make([]int, owners)
	for trial := range trials {
		claimed := map[string]int{}
		for owner := range owners {
			shuffle := seededShuffle(uint64(trial*owners + owner))
			if !spread {
				shuffle = nil
			}
			picks := orderCandidates(next, nil, window, shuffle)[:launchLimit]
			for _, key := range picks {
				attempts++
				if _, taken := claimed[key.Name()]; taken {
					lost++
					continue
				}
				claimed[key.Name()] = owner
				wins[owner]++
			}
		}
	}

	total := 0
	for _, w := range wins {
		total += w
	}
	shares := make([]float64, owners)
	for i, w := range wins {
		shares[i] = float64(w) / float64(total)
	}
	return float64(lost) / float64(attempts), shares
}

func TestOrderCandidates_ThreeOwnerContentionModel(t *testing.T) {
	const (
		owners = contentionOwners
		// The aggregate a correct window produces is about 2%. The bar sits
		// between that and the ~4.5% a halved window produces, so shrinking
		// the window is visible rather than absorbed by slack.
		maxLostRate    = 0.035
		shareTolerance = 0.06
	)

	for _, launchLimit := range []int{1, 2} {
		t.Run(fmt.Sprintf("launch_limit_%d", launchLimit), func(t *testing.T) {
			lostRate, shares := contentionRates(t, launchLimit, DefaultCandidateWindowFactor*launchLimit, true)
			if lostRate >= maxLostRate {
				t.Errorf("lost %.1f%% of claim attempts, want under %.0f%%", lostRate*100, maxLostRate*100)
			}
			even := 1.0 / float64(owners)
			for owner, share := range shares {
				if delta := (share - even) / even; delta > shareTolerance || delta < -shareTolerance {
					t.Errorf("owner %d claimed %.1f%% of successful claims, want within %.0f%% of %.1f%%",
						owner, share*100, shareTolerance*100, even*100)
				}
			}

			// Head-first selection is what this replaces: every owner reads the
			// same order, so only one of each pass's attempts survives.
			headFirst, _ := contentionRates(t, launchLimit, DefaultCandidateWindowFactor*launchLimit, false)
			if want := float64(owners-1) / float64(owners); headFirst != want {
				t.Errorf("head-first lost %.3f of attempts, want exactly %.3f", headFirst, want)
			}

			// Halving the window has to be visible, or the bar is slack
			// enough that a materially worse sizing would ship.
			if halved, _ := contentionRates(t, launchLimit, (DefaultCandidateWindowFactor/2)*launchLimit, true); halved < maxLostRate {
				t.Errorf("halving the window still lost only %.1f%%, under the %.0f%% bar, so the bar does not discriminate",
					halved*100, maxLostRate*100)
			}

			// A window of 4x the launch limit is the sizing this rejects: it
			// must fail the same assertions, or they prove nothing.
			narrow, narrowShares := contentionRates(t, launchLimit, 4*launchLimit, true)
			if narrow < maxLostRate {
				t.Errorf("a 4x window lost only %.1f%% of attempts, the threshold does not discriminate", narrow*100)
			}
			if delta := (narrowShares[0] - even) / even; delta <= shareTolerance {
				t.Errorf("a 4x window kept owner 0 at %.1f%%, the share tolerance does not discriminate", narrowShares[0]*100)
			}
		})
	}
}

// --- Candidate ordering through HandleAsync ---

// withShuffle injects a deterministic permutation source so a dispatch pass
// can be asserted on.
func withShuffle(shuffle func(n int, swap func(i, j int))) Option {
	return func(c *config) { c.shuffle = shuffle }
}

// startedKeys reports which of keys the dispatcher ran the callback for.
func startedKeys(keys []workqueue.QueuedKey) []string {
	var out []string
	for _, k := range keys {
		if mk, ok := k.(*mockKey); ok && mk.complete > 0 {
			out = append(out, mk.name)
		}
	}
	return out
}

func TestHandleAsync_NoIdentityKeepsHeadFirst(t *testing.T) {
	next := queuedKeys(40)
	q := &mockQueue{next: next}

	future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0, withShuffle(seededShuffle(3)))
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if diff := cmp.Diff([]string{"k0"}, startedKeys(next)); diff != "" {
		t.Fatalf("a queue with no identity did not take the head (-want +got):\n%s", diff)
	}
}

func TestHandleAsync_SpreadStaysInWindow(t *testing.T) {
	// The queue has to be longer than the window or there is nothing outside
	// it to catch, so size it from the constant rather than hard-coding it.
	const queued = DefaultCandidateWindowFactor * 2

	started := map[string]int{}
	// Enough seeds that an implementation ignoring the window, which would
	// reach beyond it in half of all passes, cannot clear them all.
	for seed := uint64(1); seed <= 50; seed++ {
		next := queuedKeys(queued)
		q := &mockQueue{identity: "us-central1", next: next}

		future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
			return nil
		}, 0, withShuffle(seededShuffle(seed)))
		if err := future(); err != nil {
			t.Fatalf("seed %d: unexpected error: %v", seed, err)
		}

		got := startedKeys(next)
		if len(got) != 1 {
			t.Fatalf("seed %d: started %v, want exactly one key", seed, got)
		}
		for i := DefaultCandidateWindowFactor; i < queued; i++ {
			if got[0] == fmt.Sprintf("k%d", i) {
				t.Fatalf("seed %d: started %q, which is outside the window of %d", seed, got[0], DefaultCandidateWindowFactor)
			}
		}
		started[got[0]]++
	}
	if len(started) == 1 {
		t.Fatalf("every seed started the same key %v, the pass is not being spread", started)
	}
}

func TestHandleAsync_WindowScalesWithLaunchLimit(t *testing.T) {
	// The window is the factor times what the pass can launch, not times the
	// batch size and not the bare factor. Two cases are needed: one where
	// launchLimit is smaller than batchSize, which catches scaling by the
	// batch size, and one where launchLimit exceeds 1, which catches dropping
	// the multiplier altogether. With launchLimit 1 the two are identical, so
	// the first case alone proves nothing about the multiplier.
	t.Run("reaches_beyond_the_bare_factor", func(t *testing.T) {
		// 36 of 40 slots held, so launchLimit is 4 and the window is 4 times
		// the factor. Keys past the bare factor must be reachable, or the
		// multiplier has been dropped.
		wip := make([]workqueue.ObservedInProgressKey, 36)
		for i := range wip {
			wip[i] = &mockInProgressKey{mockKey: &mockKey{name: fmt.Sprintf("wip%d", i)}}
		}
		queued := DefaultCandidateWindowFactor * 4
		beyondBareFactor := false
		for seed := range uint64(50) {
			next := queuedKeys(queued)
			q := &mockQueue{identity: "us-central1", wip: wip, next: next}

			future := HandleAsync(t.Context(), q, 40, 40, func(context.Context, string, workqueue.Options) error {
				return nil
			}, 0, withShuffle(seededShuffle(seed)))
			if err := future(); err != nil {
				t.Fatalf("seed %d: unexpected error: %v", seed, err)
			}
			for _, name := range startedKeys(next) {
				n, err := strconv.Atoi(strings.TrimPrefix(name, "k"))
				if err != nil {
					t.Fatalf("seed %d: unexpected key %q", seed, name)
				}
				if n >= queued {
					t.Fatalf("seed %d: started %q, past the whole queue", seed, name)
				}
				if n >= DefaultCandidateWindowFactor {
					beyondBareFactor = true
				}
			}
		}
		if !beyondBareFactor {
			t.Fatalf("across 50 seeds no key past k%d was ever started, so the window is not scaled by launchLimit",
				DefaultCandidateWindowFactor-1)
		}
	})

	// 39 of 40 slots are held, so batchSize is 40 but only one key can launch
	// and the window must be the factor, not 40 times it.
	wip := make([]workqueue.ObservedInProgressKey, 39)
	for i := range wip {
		wip[i] = &mockInProgressKey{mockKey: &mockKey{name: fmt.Sprintf("wip%d", i)}}
	}
	inWindow := map[string]struct{}{}
	for i := range DefaultCandidateWindowFactor {
		inWindow[fmt.Sprintf("k%d", i)] = struct{}{}
	}

	for seed := range uint64(25) {
		next := queuedKeys(200)
		q := &mockQueue{identity: "us-central1", wip: wip, next: next}

		future := HandleAsync(t.Context(), q, 40, 40, func(context.Context, string, workqueue.Options) error {
			return nil
		}, 0, withShuffle(seededShuffle(seed)))
		if err := future(); err != nil {
			t.Fatalf("seed %d: unexpected error: %v", seed, err)
		}

		got := startedKeys(next)
		if len(got) != 1 {
			t.Fatalf("seed %d: started %v, want exactly one key", seed, got)
		}
		if _, ok := inWindow[got[0]]; !ok {
			t.Fatalf("seed %d: started %q, outside the %d-key window that launchLimit 1 allows; a window scaled by batchSize would reach it",
				seed, got[0], DefaultCandidateWindowFactor)
		}
	}
}

func TestHandleAsync_PriorityAcrossOwners(t *testing.T) {
	// hi-0 is queued and already in progress, the state GCS leaves behind when
	// the winner of a claim fails to delete the queued object.
	newKeys := func() []workqueue.QueuedKey {
		return []workqueue.QueuedKey{
			&mockKey{name: "hi-0", priority: 10},
			&mockKey{name: "hi-1", priority: 10},
			&mockKey{name: "hi-2", priority: 10},
			&mockKey{name: "hi-3", priority: 10},
			&mockKey{name: "lo-0"},
			&mockKey{name: "lo-1"},
			&mockKey{name: "lo-2"},
			&mockKey{name: "lo-3"},
		}
	}
	wip := func() []workqueue.ObservedInProgressKey {
		return []workqueue.ObservedInProgressKey{
			&mockInProgressKey{mockKey: &mockKey{name: "hi-0", owner: "us-east4"}},
		}
	}

	t.Run("lower_priority_untouched_while_higher_remains", func(t *testing.T) {
		for i, identity := range []string{"us-central1", "us-east4"} {
			next := newKeys()
			q := &mockQueue{identity: identity, wip: wip(), next: next}

			// Three eligible high-priority keys against a limit of two, so no
			// owner should ever reach the lower-priority run.
			future := HandleAsync(t.Context(), q, 3, 2, func(context.Context, string, workqueue.Options) error {
				return nil
			}, 0, withShuffle(seededShuffle(uint64(i))))
			if err := future(); err != nil {
				t.Fatalf("%s: unexpected error: %v", identity, err)
			}

			got := startedKeys(next)
			if len(got) != 2 {
				t.Fatalf("%s: started %v, want two keys", identity, got)
			}
			for _, name := range got {
				if name == "hi-0" {
					t.Fatalf("%s: started %q, which is already in progress", identity, name)
				}
				if name[:2] == "lo" {
					t.Fatalf("%s: started %q while eligible high-priority keys remained: %v", identity, name, got)
				}
			}
		}
	})

	t.Run("spills_into_lower_priority_only_when_exhausted", func(t *testing.T) {
		next := newKeys()
		// Drop hi-2 and hi-3 so only two high-priority keys are eligible.
		next = append(next[:2:2], next[4:]...)
		q := &mockQueue{identity: "us-central1", wip: wip(), next: next}

		// hi-0 holds one of the four slots, leaving three to launch: the one
		// eligible high-priority key and then two from the lower run.
		future := HandleAsync(t.Context(), q, 4, 0, func(context.Context, string, workqueue.Options) error {
			return nil
		}, 0, withShuffle(seededShuffle(11)))
		if err := future(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := startedKeys(next)
		high, low := 0, 0
		for _, name := range got {
			if name[:2] == "hi" {
				high++
			} else {
				low++
			}
		}
		if high != 1 || low != 2 {
			t.Fatalf("started %v (%d high, %d low), want the one eligible high key plus two low", got, high, low)
		}
	})
}

// claimSet is the shared in-progress set of a queue several dispatchers are
// claiming from: the first Start for a key wins and the rest fail the way a
// lost precondition does.
type claimSet struct {
	mu      sync.Mutex
	claimed map[string]struct{}
	lost    int
}

type contendedKey struct {
	*mockKey
	claims *claimSet
}

func (c *contendedKey) Start(context.Context) (workqueue.OwnedInProgressKey, error) {
	c.claims.mu.Lock()
	defer c.claims.mu.Unlock()
	if _, taken := c.claims.claimed[c.name]; taken {
		c.claims.lost++
		return nil, errors.New("copy to in-progress failed: precondition not met")
	}
	c.claims.claimed[c.name] = struct{}{}
	return &mockInProgressKey{mockKey: c.mockKey}, nil
}

func TestHandleAsync_ThreeOwnersContend(t *testing.T) {
	const (
		trials      = 200
		launchLimit = 2
	)
	identities := []string{"us-central1", "us-east4", "us-west1"}

	contend := func(spread bool) float64 {
		attempts, lost := 0, 0
		for trial := range trials {
			claims := &claimSet{claimed: map[string]struct{}{}}
			for owner, identity := range identities {
				next := make([]workqueue.QueuedKey, 200)
				for i := range next {
					next[i] = &contendedKey{mockKey: &mockKey{name: fmt.Sprintf("k%d", i)}, claims: claims}
				}
				q := &mockQueue{next: next}
				if spread {
					q.identity = identity
				}

				future := HandleAsync(t.Context(), q, launchLimit, 0, func(context.Context, string, workqueue.Options) error {
					return nil
				}, 0, withShuffle(seededShuffle(uint64(trial*len(identities)+owner))))
				if err := future(); err != nil {
					t.Fatalf("trial %d, owner %s: unexpected error: %v", trial, identity, err)
				}
				attempts += launchLimit
			}
			lost += claims.lost
		}
		return float64(lost) / float64(attempts)
	}

	if got := contend(true); got >= 0.05 {
		t.Errorf("three spread dispatchers lost %.1f%% of claim attempts, want under 5%%", got*100)
	}
	// Without an identity every dispatcher takes the same head, so only the
	// first of each key's claims survives: 4 of every 6 attempts are lost.
	if got, want := contend(false), 4.0/6.0; got != want {
		t.Errorf("three head-first dispatchers lost %.3f of claim attempts, want exactly %.3f", got, want)
	}
}

func TestHandleAsync_DefaultShuffleSpreads(t *testing.T) {
	// No withShuffle here: this is the only test that exercises the source a
	// deployed dispatcher uses.
	started := map[string]int{}
	for range 20 {
		next := queuedKeys(40)
		q := &mockQueue{identity: "us-central1", next: next}

		future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
			return nil
		}, 0)
		if err := future(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := startedKeys(next)
		if len(got) != 1 {
			t.Fatalf("started %v, want exactly one key", got)
		}
		started[got[0]]++
	}
	if len(started) == 1 {
		t.Fatalf("20 passes all started %v, so no shuffle source is wired in by default", started)
	}
}

func TestHandleAsync_ZeroWindowFactorTakesTheHead(t *testing.T) {
	// The per-env rollout switch. A factor of zero has to leave dispatch
	// exactly as it was before candidate spreading existed, even though the
	// queue has an identity and a shuffle source is available.
	for seed := range uint64(25) {
		next := queuedKeys(200)
		q := &mockQueue{identity: "us-central1", next: next}

		future := HandleAsync(t.Context(), q, 1, 0, func(context.Context, string, workqueue.Options) error {
			return nil
		}, 0, withShuffle(seededShuffle(seed)), WithCandidateWindowFactor(0))
		if err := future(); err != nil {
			t.Fatalf("seed %d: unexpected error: %v", seed, err)
		}

		if diff := cmp.Diff([]string{"k0"}, startedKeys(next)); diff != "" {
			t.Fatalf("seed %d: a zero window factor did not take the head (-want +got):\n%s", seed, diff)
		}
	}
}

func TestApplyOptions_CandidateWindowFactor(t *testing.T) {
	if got := applyOptions(nil).windowFactor; got != DefaultCandidateWindowFactor {
		t.Errorf("default window factor = %d, want %d", got, DefaultCandidateWindowFactor)
	}
	if got := applyOptions([]Option{WithCandidateWindowFactor(0)}).windowFactor; got != 0 {
		t.Errorf("WithCandidateWindowFactor(0) = %d, want 0", got)
	}
	if got := applyOptions([]Option{WithCandidateWindowFactor(8)}).windowFactor; got != 8 {
		t.Errorf("WithCandidateWindowFactor(8) = %d, want 8", got)
	}
}

func TestOrderCandidates_NeverPromotesPastTheWindow(t *testing.T) {
	// The property that rules out starvation by arrival. Keys enter the queue
	// ordered oldest-first, so a key that arrives later sorts behind one that
	// is already waiting. The shuffle must not let a later key overtake an
	// earlier one from outside the window, or a busy queue could keep feeding
	// new work past a key that never gets its turn.
	const window = DefaultCandidateWindowFactor
	next := queuedKeys(window * 4)
	for seed := range uint64(200) {
		got := orderCandidates(next, nil, window, seededShuffle(seed))
		if len(got) != len(next) {
			t.Fatalf("seed %d: got %d candidates, want %d", seed, len(got), len(next))
		}
		// Everything the shuffle can reach came from the oldest window, and
		// the remainder is untouched, so position only improves with age.
		for i, k := range got[:window] {
			if n, _ := strconv.Atoi(strings.TrimPrefix(k.Name(), "k")); n >= window {
				t.Fatalf("seed %d: %q from beyond the window appeared at position %d", seed, k.Name(), i)
			}
		}
		for i, k := range got[window:] {
			if want := fmt.Sprintf("k%d", i+window); k.Name() != want {
				t.Fatalf("seed %d: tail position %d is %q, want %q", seed, i+window, k.Name(), want)
			}
		}
	}
}

func TestHandleAsync_OldestKeyStartsUnderSustainedArrivals(t *testing.T) {
	// Liveness. Arrivals run well ahead of what the dispatcher can start, so
	// the backlog grows without bound. The key that is oldest at the start
	// must still be dispatched, and its wait must not grow with the arrival
	// rate, because new work sorts behind it.
	const (
		launchLimit = 2
		bound       = 1000
		// The measured worst wait is ~209 passes at every arrival rate. This
		// bounds it with headroom rather than merely asserting the key is
		// eventually reached, which a window-free implementation also
		// satisfies.
		worstAllowed = 500
	)
	// Arrival rates at and above the drain rate. Rates below it are not
	// interesting: the backlog shrinks and every key is reached quickly.
	for _, arrivalsPerPass := range []int{5, 20} {
		t.Run(fmt.Sprintf("arrivals_%d", arrivalsPerPass), func(t *testing.T) {
			worst := 0
			for seed := range uint64(20) {
				queue := queuedKeys(200)
				oldest := queue[0].Name()
				created := len(queue)
				started := ""

				for pass := 1; pass <= bound && started == ""; pass++ {
					q := &mockQueue{identity: "us-central1", next: queue}
					future := HandleAsync(t.Context(), q, launchLimit, 0, func(context.Context, string, workqueue.Options) error {
						return nil
					}, 0, withShuffle(seededShuffle(seed*uint64(bound)+uint64(pass))))
					if err := future(); err != nil {
						t.Fatalf("seed %d pass %d: unexpected error: %v", seed, pass, err)
					}

					remaining := make([]workqueue.QueuedKey, 0, len(queue)+arrivalsPerPass)
					for _, k := range queue {
						if mk, ok := k.(*mockKey); ok && mk.complete > 0 {
							if mk.name == oldest {
								started = mk.name
								worst = max(worst, pass)
							}
							continue
						}
						remaining = append(remaining, k)
					}
					// New work always sorts behind what is already waiting.
					for range arrivalsPerPass {
						remaining = append(remaining, &mockKey{name: fmt.Sprintf("k%d", created)})
						created++
					}
					queue = remaining
				}

				if started == "" {
					t.Fatalf("seed %d: the oldest key was never started in %d passes with %d arrivals per pass",
						seed, bound, arrivalsPerPass)
				}
			}
			if worst > worstAllowed {
				t.Errorf("arrivals %d per pass: worst wait %d passes, want at most %d; the wait is growing with the arrival rate",
					arrivalsPerPass, worst, worstAllowed)
			}
			t.Logf("arrivals %d per pass: worst observed wait %d passes", arrivalsPerPass, worst)
		})
	}
}

func TestHandleAsync_SkipsInProgressKeysWithoutAnIdentity(t *testing.T) {
	// A queue with no identity never reaches orderCandidates, so the launch
	// loop's own skip is the only thing keeping it from starting a key that
	// is already in progress. GCS leaves a key in both states when the winner
	// of a claim fails to delete the queued object.
	next := queuedKeys(3)
	q := &mockQueue{
		next: next,
		wip: []workqueue.ObservedInProgressKey{
			&mockInProgressKey{mockKey: &mockKey{name: "k0"}},
		},
	}

	future := HandleAsync(t.Context(), q, 4, 0, func(context.Context, string, workqueue.Options) error {
		return nil
	}, 0)
	if err := future(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if diff := cmp.Diff([]string{"k1", "k2"}, startedKeys(next)); diff != "" {
		t.Fatalf("the in-progress key was started again (-want +got):\n%s", diff)
	}
}
