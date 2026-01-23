/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package ocireconciler_test

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-containerregistry/pkg/name"
)

// ExampleNew demonstrates creating a basic reconciler that processes
// OCI image digests from a workqueue.
func ExampleNew() {
	r := ocireconciler.New(
		ocireconciler.WithReconciler(func(_ context.Context, digest name.Digest) error {
			fmt.Printf("Processing: %s\n", digest.DigestStr())
			return nil
		}),
	)

	// Process a digest directly (normally done via gRPC workqueue)
	ctx := context.Background()
	err := r.Reconcile(ctx, "cgr.dev/example@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae")
	if err != nil {
		panic(err)
	}
	// Output: Processing: sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae
}

// ExampleWithNameOptions demonstrates configuring digest parsing options,
// such as setting a default registry for unqualified references.
func ExampleWithNameOptions() {
	var registry string
	r := ocireconciler.New(
		ocireconciler.WithNameOptions(name.WithDefaultRegistry("gcr.io")),
		ocireconciler.WithReconciler(func(_ context.Context, digest name.Digest) error {
			registry = digest.RegistryStr()
			return nil
		}),
	)

	ctx := context.Background()
	// This digest has no registry, so it uses the default
	err := r.Reconcile(ctx, "example/image@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae")
	if err != nil {
		panic(err)
	}
	fmt.Printf("Registry: %s\n", registry)
	// Output: Registry: gcr.io
}

// ExampleReconcilerFunc_requeue demonstrates returning a requeue delay
// to retry reconciliation after a specified duration.
func ExampleReconcilerFunc_requeue() {
	r := ocireconciler.New(
		ocireconciler.WithReconciler(func(context.Context, name.Digest) error {
			// Simulate rate limiting - ask to retry in 1 minute
			return workqueue.RequeueAfter(time.Minute)
		}),
	)

	ctx := context.Background()
	resp, err := r.Process(ctx, &workqueue.ProcessRequest{
		Key: "cgr.dev/example@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Requeue after: %d seconds\n", resp.RequeueAfterSeconds)
	// Output: Requeue after: 60 seconds
}

// ExampleReconcilerFunc_nonRetriable demonstrates marking an error as
// non-retriable to prevent further retry attempts.
func ExampleReconcilerFunc_nonRetriable() {
	r := ocireconciler.New(
		ocireconciler.WithReconciler(func(context.Context, name.Digest) error {
			// Permanent failure - don't retry
			return workqueue.NonRetriableError(fmt.Errorf("image deleted"), "image not found")
		}),
	)

	ctx := context.Background()
	resp, err := r.Process(ctx, &workqueue.ProcessRequest{
		Key: "cgr.dev/example@sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
	})
	if err != nil {
		panic(err)
	}
	// Non-retriable errors return success with no requeue
	fmt.Printf("Requeue after: %d seconds\n", resp.RequeueAfterSeconds)
	// Output: Requeue after: 0 seconds
}
