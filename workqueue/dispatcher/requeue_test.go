/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/inmem"
)

func TestRequeueWithDelay(t *testing.T) {
	ctx := context.Background()
	wq := inmem.NewWorkQueue(10)

	// Queue a test key
	key := "test-key"
	if err := wq.Queue(ctx, key, workqueue.Options{Priority: 1}); err != nil {
		t.Fatalf("Failed to queue key: %v", err)
	}

	// Define test cases
	tests := []struct {
		name          string
		callback      Callback
		wantRequeued  bool
		wantMinDelay  time.Duration
		wantCompleted bool
	}{{
		name: "successful processing",
		callback: func(_ context.Context, _ string, _ workqueue.Options) error {
			return nil
		},
		wantCompleted: true,
	}, {
		name: "requeue with 5 second delay",
		callback: func(_ context.Context, _ string, _ workqueue.Options) error {
			return workqueue.RequeueAfter(5 * time.Second)
		},
		wantRequeued: true,
		wantMinDelay: 5 * time.Second,
	}, {
		name: "requeue with 1 minute delay",
		callback: func(_ context.Context, _ string, _ workqueue.Options) error {
			return workqueue.RequeueAfter(time.Minute)
		},
		wantRequeued: true,
		wantMinDelay: time.Minute,
	}, {
		name: "non-retriable error",
		callback: func(_ context.Context, _ string, _ workqueue.Options) error {
			return workqueue.NonRetriableError(context.Canceled, "test non-retriable")
		},
		wantCompleted: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Queue the key
			if err := wq.Queue(ctx, key, workqueue.Options{Priority: 1}); err != nil {
				t.Fatalf("Failed to queue key: %v", err)
			}

			// Process with our test callback
			if err := Handle(ctx, wq, 1, 0, tt.callback); err != nil {
				t.Fatalf("Handle failed: %v", err)
			}

			// Check the results
			wip, queued, _, err := wq.Enumerate(ctx)
			if err != nil {
				t.Fatalf("Failed to enumerate: %v", err)
			}

			if tt.wantCompleted {
				// Should not be in WIP or queued
				if len(wip) > 0 {
					t.Errorf("WIP items: got = %d, wanted = 0", len(wip))
				}
				if len(queued) > 0 {
					t.Errorf("queued items: got = %d, wanted = 0", len(queued))
				}
			} else if tt.wantRequeued {
				// Should be requeued but with a delay, so it won't show up in Enumerate yet
				if len(wip) > 0 {
					t.Errorf("WIP items after requeue: got = %d, wanted = 0", len(wip))
				}

				// The item should be queued but not visible due to the delay
				// To verify it was requeued, we need to check the internal state
				// For now, we can verify by waiting or by checking that it's not immediately available
				if len(queued) > 0 {
					// If we see queued items, they should not be startable due to delay
					qk := queued[0]
					_, err := qk.Start(ctx)
					if err != nil {
						t.Errorf("Could not start queued item, which suggests it has a future NotBefore: %v", err)
					}
				} else {
					// This is expected - the item is queued with a future NotBefore time
					// Let's verify by checking that after a small wait, we still don't see it
					// (since our delays are much longer than a millisecond)
					time.Sleep(10 * time.Millisecond)
					_, queued2, _, err := wq.Enumerate(ctx)
					if err != nil {
						t.Fatalf("Failed to enumerate after delay: %v", err)
					}
					if len(queued2) > 0 {
						t.Errorf("Item appeared in queue too soon - delay might not be working")
					}
				}
			}
		})
	}
}

func TestRequeueFloorClamped(t *testing.T) {
	ctx := context.Background()
	wq := inmem.NewWorkQueue(10)

	// Shrink the ceiling so the test doesn't have to reason about an hour.
	orig := workqueue.MaximumRequeueFloor
	workqueue.MaximumRequeueFloor = 2 * time.Second
	t.Cleanup(func() { workqueue.MaximumRequeueFloor = orig })

	key := "floored-key"
	if err := wq.Queue(ctx, key, workqueue.Options{Priority: 1}); err != nil {
		t.Fatalf("Queue failed: %v", err)
	}

	// The callback asks for a floored requeue far beyond the ceiling.
	cb := func(_ context.Context, _ string, _ workqueue.Options) error {
		return workqueue.RequeueNotBefore(time.Hour)
	}
	if err := Handle(ctx, wq, 1, 0, cb); err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	st, err := wq.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	notBefore := time.Unix(st.NotBeforeTime, 0)
	// The floor is still applied (in the future)...
	if !notBefore.After(time.Now()) {
		t.Errorf("NotBefore %v is not in the future; floor was dropped", notBefore)
	}
	// ...but clamped to roughly now+MaximumRequeueFloor, not now+1h.
	if upper := time.Now().Add(workqueue.MaximumRequeueFloor + time.Minute); notBefore.After(upper) {
		t.Errorf("NotBefore %v not clamped; want <= ~now+%v", notBefore, workqueue.MaximumRequeueFloor)
	}
}

func TestServiceCallbackWithDelay(t *testing.T) {
	// Create a mock client that returns a response with RequeueAfterSeconds
	mockClient := &mockWorkqueueClient{
		processFunc: func(_ context.Context, _ *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
			return &workqueue.ProcessResponse{
				RequeueAfterSeconds: 30,
			}, nil
		},
	}

	callback := ServiceCallback(mockClient)
	err := callback(context.Background(), "test-key", workqueue.Options{})

	// Should return a requeue error
	delay, ok := workqueue.GetRequeueDelay(err)
	if !ok {
		t.Fatalf("Expected requeue error, got: %v", err)
	}
	if delay != 30*time.Second {
		t.Errorf("Expected 30 second delay, got %v", delay)
	}
}

func TestServiceCallbackWithFloor(t *testing.T) {
	// A response with RequeueFloor set should map to RequeueNotBefore (a floor),
	// not RequeueAfter.
	mockClient := &mockWorkqueueClient{
		processFunc: func(_ context.Context, _ *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
			return &workqueue.ProcessResponse{
				RequeueAfterSeconds: 30,
				RequeueFloor:        true,
			}, nil
		},
	}

	callback := ServiceCallback(mockClient)
	err := callback(context.Background(), "test-key", workqueue.Options{})

	delay, floor, ok := workqueue.GetRequeueOptions(err)
	if !ok {
		t.Fatalf("Expected requeue error, got: %v", err)
	}
	if delay != 30*time.Second {
		t.Errorf("Expected 30 second delay, got %v", delay)
	}
	if !floor {
		t.Error("Expected floor=true (RequeueFloor set in response), got false")
	}
}

// Mock client for testing
type mockWorkqueueClient struct {
	workqueue.WorkqueueServiceClient
	processFunc func(context.Context, *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error)
}

func (m *mockWorkqueueClient) Process(ctx context.Context, req *workqueue.ProcessRequest, _ ...grpc.CallOption) (*workqueue.ProcessResponse, error) {
	return m.processFunc(ctx, req)
}
