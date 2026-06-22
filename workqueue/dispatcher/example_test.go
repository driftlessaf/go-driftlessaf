/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher_test

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/driftlessaf/workqueue/dispatcher"
	"chainguard.dev/driftlessaf/workqueue/inmem"
)

// ExampleHandle demonstrates dispatching a single round of work from a
// workqueue using Handle.
func ExampleHandle() {
	wq := inmem.NewWorkQueue(5)
	ctx := context.Background()

	if err := wq.Queue(ctx, "example-key", workqueue.Options{}); err != nil {
		panic(err)
	}

	processed := false
	err := dispatcher.Handle(ctx, wq, 5, 0, func(_ context.Context, key string, _ workqueue.Options) error {
		fmt.Printf("Processing key: %s\n", key)
		processed = true
		return nil
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Processed: %v\n", processed)
	// Output:
	// Processing key: example-key
	// Processed: true
}

// ExampleServiceCallback demonstrates creating a Callback that delegates to a
// WorkqueueServiceClient.
func ExampleServiceCallback() {
	// ServiceCallback wraps a gRPC WorkqueueServiceClient as a dispatcher Callback.
	// In production, pass a real client obtained from a gRPC connection.
	var client workqueue.WorkqueueServiceClient
	cb := dispatcher.ServiceCallback(client)
	_ = cb
	fmt.Println("ServiceCallback created")
	// Output: ServiceCallback created
}

// ExampleWithErrorIngressURI demonstrates enabling error events.
// When the broker URL is empty, error events are silently disabled.
func ExampleWithErrorIngressURI() {
	wq := inmem.NewWorkQueue(5)
	ctx := context.Background()

	if err := wq.Queue(ctx, "example-key", workqueue.Options{}); err != nil {
		panic(err)
	}

	// Empty URL disables error events (no-op).
	err := dispatcher.Handle(ctx, wq, 5, 0, func(_ context.Context, _ string, _ workqueue.Options) error {
		return fmt.Errorf("something went wrong")
	}, dispatcher.WithErrorIngressURI(ctx, "", "my-workqueue"))
	if err != nil {
		panic(err)
	}
	fmt.Println("dispatched with error events disabled")
	// Output:
	// dispatched with error events disabled
}

// ExampleWithBackoff demonstrates installing a failure-retry backoff. The hook
// receives the key's attempt count and returns the delay before the next retry;
// here it grows linearly with attempts. Returning a non-positive duration (or
// leaving the option unset) keeps the default bare-requeue behavior.
func ExampleWithBackoff() {
	wq := inmem.NewWorkQueue(5)
	ctx := context.Background()

	if err := wq.Queue(ctx, "example-key", workqueue.Options{}); err != nil {
		panic(err)
	}

	// Back off two seconds per attempt before the next retry.
	backoff := func(attempts int) time.Duration {
		return time.Duration(attempts) * 2 * time.Second
	}

	err := dispatcher.Handle(ctx, wq, 5, 0, func(_ context.Context, _ string, _ workqueue.Options) error {
		return fmt.Errorf("something went wrong")
	}, dispatcher.WithBackoff(backoff))
	if err != nil {
		panic(err)
	}
	fmt.Println("dispatched with failure-retry backoff")
	// Output:
	// dispatched with failure-retry backoff
}
