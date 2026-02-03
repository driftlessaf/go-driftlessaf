/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"errors"
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
	orphaned bool
	startErr error
	attempts int
	requeue  int
	dead     int
	complete int
	mu       sync.Mutex
}

// Implement Priority() to satisfy workqueue.QueuedKey.
func (m *mockKey) Priority() int64 {
	return 0
}

func (m *mockKey) Name() string     { return m.name }
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
	return nil
}

func (m *mockKey) RequeueWithOptions(context.Context, workqueue.Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requeue++
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
	return nil
}

type queuedItem struct {
	key  string
	opts workqueue.Options
}

type mockQueue struct {
	mu      sync.Mutex
	wip     []workqueue.ObservedInProgressKey
	next    []workqueue.QueuedKey
	err     error
	queued  []queuedItem
	failKey string // If set, Queue will fail for this key
}

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

func TestHandleAsync_RespectsBatchSize(t *testing.T) {
	keys := []*mockKey{
		{name: "k1"},
		{name: "k2"},
		{name: "k3"},
	}

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

// TestHandleAsync_RequeueSucceedsWithCancelledContext tests that cleanup operations
// (Requeue, Complete, Deadletter) succeed even when the parent context is cancelled.
// This is critical for graceful shutdown - when Cloud Run sends SIGTERM, we need to
// ensure work items are properly requeued rather than left stuck in "in-progress" state.
func TestHandleAsync_RequeueSucceedsWithCancelledContext(t *testing.T) {
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
		t.Errorf("expected Requeue to be called even with cancelled context, got requeue=%d", next.requeue)
	}
}

// TestHandleAsync_CompleteSucceedsWithCancelledContext tests that Complete succeeds
// even when the parent context is cancelled during successful work completion.
func TestHandleAsync_CompleteSucceedsWithCancelledContext(t *testing.T) {
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
		t.Errorf("expected Complete to be called even with cancelled context, got complete=%d", next.complete)
	}
}

// TestHandleAsync_OrphanRequeueSucceedsWithCancelledContext tests that orphaned work
// requeue succeeds even when the context is cancelled.
func TestHandleAsync_OrphanRequeueSucceedsWithCancelledContext(t *testing.T) {
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

	// Critical: Orphan requeue should succeed despite cancelled context
	if orphan.requeue != 1 {
		t.Errorf("expected orphaned key requeue even with cancelled context, got requeue=%d", orphan.requeue)
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
			t.Errorf("queued[%d].key = %q, want %q", i, queued[i].key, want)
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
		t.Errorf("queued[0].opts.Priority = %d, want 100", queued[0].opts.Priority)
	}
	if queued[1].opts.Priority != 10 {
		t.Errorf("queued[1].opts.Priority = %d, want 10", queued[1].opts.Priority)
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
		t.Errorf("NotBefore too early: got = %v, want >= %v", queued[0].opts.NotBefore, expectedNotBefore)
	}
	if queued[0].opts.NotBefore.After(maxNotBefore) {
		t.Errorf("NotBefore too late: got = %v, want <= %v", queued[0].opts.NotBefore, maxNotBefore)
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
		t.Errorf("queued key = %q, want %q", queued[0].key, "self-requeue")
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
			t.Errorf("queued[%d].key = %q, want %q", i, queued[i].key, want)
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
		t.Errorf("queued key = %q, want %q", queued[0].key, "child1")
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
		t.Errorf("delay = %v, want 30s", delay)
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
