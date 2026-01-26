/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package ocireconciler provides a workqueue-based reconciliation framework for OCI image digests.
//
// This package enables building reconcilers that process container image digests from a
// workqueue, similar to how githubreconciler handles GitHub resources. The key difference
// is that ocireconciler uses immutable image digests as the unit of work, making them
// ideal for reconciliation patterns where the digest itself represents the generation.
//
// # Basic Usage
//
// Create a reconciler by providing a ReconcilerFunc that processes each digest:
//
//	r := ocireconciler.NewReconciler(
//	    ocireconciler.WithReconciler(func(ctx context.Context, digest name.Digest) error {
//	        // Process the digest - fetch manifest, verify signatures, etc.
//	        desc, err := remote.Get(digest)
//	        if err != nil {
//	            return fmt.Errorf("fetching manifest: %w", err)
//	        }
//	        log.Printf("Processing %s with digest %s", digest.Repository, digest.DigestStr())
//	        return nil
//	    }),
//	)
//
// # Workqueue Integration
//
// The Reconciler implements the WorkqueueServiceServer interface, making it easy
// to deploy as a regional-go-reconciler:
//
//	workqueue.RegisterWorkqueueServiceServer(grpcServer, reconciler)
//
// Keys enqueued to the workqueue should be fully-qualified digest references like
// "cgr.dev/chainguard/static@sha256:abc123...".
//
// # Error Handling
//
// The reconciler supports workqueue error semantics:
//
//   - Return nil for successful reconciliation
//   - Return workqueue.RequeueAfter(duration) to retry after a delay
//   - Return workqueue.NonRetriableError(err, reason) for permanent failures
//   - Return other errors for transient failures that should be retried
//
// Example with error handling:
//
//	ocireconciler.WithReconciler(func(ctx context.Context, digest name.Digest) error {
//	    desc, err := remote.Get(digest)
//	    if err != nil {
//	        if isRateLimited(err) {
//	            return workqueue.RequeueAfter(time.Minute)
//	        }
//	        if isNotFound(err) {
//	            return workqueue.NonRetriableError(err, "image not found")
//	        }
//	        return err // Transient error, will retry
//	    }
//	    return nil
//	})
//
// # Registry Configuration
//
// Use WithNameOptions to configure digest parsing behavior, such as allowing
// insecure registries or setting a default registry:
//
//	r := ocireconciler.NewReconciler(
//	    ocireconciler.WithNameOptions(name.Insecure),
//	    ocireconciler.WithReconciler(myReconcileFunc),
//	)
//
// # Status Management
//
// For reconcilers that need to track status across invocations, use the
// statusmanager subpackage to persist reconciliation state as OCI attestations.
// This enables idempotent reconciliation by comparing observed vs desired state.
//
// # Thread Safety
//
// The Reconciler is safe for concurrent use. Multiple Process calls can execute
// simultaneously, each with its own context and digest.
package ocireconciler
