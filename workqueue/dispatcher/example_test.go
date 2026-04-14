/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package dispatcher_test

import (
	"context"
	"fmt"

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

// ExampleWithErrorBrokerURL demonstrates enabling error events.
// When the broker URL is empty, error events are silently disabled.
func ExampleWithErrorBrokerURL() {
	wq := inmem.NewWorkQueue(5)
	ctx := context.Background()

	if err := wq.Queue(ctx, "example-key", workqueue.Options{}); err != nil {
		panic(err)
	}

	// Empty URL disables error events (no-op).
	err := dispatcher.Handle(ctx, wq, 5, 0, func(_ context.Context, _ string, _ workqueue.Options) error {
		return fmt.Errorf("something went wrong")
	}, dispatcher.WithErrorBrokerURL(ctx, "", "my-workqueue"))
	if err != nil {
		panic(err)
	}
	fmt.Println("dispatched with error events disabled")
	// Output:
	// dispatched with error events disabled
}
