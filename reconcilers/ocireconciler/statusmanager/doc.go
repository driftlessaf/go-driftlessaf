/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package statusmanager persists ocireconciler reconciliation state as OCI attestations.
//
// This package provides a mechanism for storing and retrieving reconciliation status
// as signed OCI attestations, using Sigstore's keyless signing with Fulcio and Rekor.
// It mirrors the githubreconciler/statusmanager pattern but targets OCI digests and
// stores progress directly as attestations rather than GitHub check runs.
//
// # Key Features
//
//   - Keyless signing using Fulcio certificates and Rekor transparency log
//   - Identity-scoped attestations with predicate type "https://statusmanager.chainguard.dev/{identity}"
//   - REPLACE semantics ensuring exactly one attestation per digest/predicate pair
//   - Full signature verification on read using cosign verification
//   - Support for repository override (similar to COSIGN_REPOSITORY)
//
// # Basic Usage
//
// Create a Manager and use sessions to track reconciliation state:
//
//	// Create a manager (requires GCP service account credentials)
//	mgr, err := statusmanager.New[MyDetails](ctx, "my-reconciler")
//	if err != nil {
//	    return err
//	}
//
//	// Start a session for a specific digest
//	session := mgr.NewSession(digest)
//
//	// Check previous state
//	observed, err := session.ObservedState(ctx)
//	if err != nil {
//	    return err
//	}
//	if observed != nil {
//	    log.Printf("Previous state: %+v", observed.Details)
//	}
//
//	// Perform reconciliation work...
//
//	// Record new state
//	err = session.SetActualState(ctx, &statusmanager.Status[MyDetails]{
//	    Details: MyDetails{Result: "success"},
//	})
//
// # Status Type
//
// The Status type is generic over the Details field, allowing you to store
// arbitrary structured data alongside the automatically-populated ObservedGeneration:
//
//	type Status[T any] struct {
//	    ObservedGeneration string // Automatically set to the digest
//	    Details            T      // Your custom status data
//	}
//
// # Repository Override
//
// When the subject image is in a registry that doesn't support attestations,
// or when you want to store attestations separately, use WithRepositoryOverride:
//
//	mgr, err := statusmanager.New[MyDetails](ctx, "my-reconciler",
//	    statusmanager.WithRepositoryOverride("gcr.io/my-project/attestations"),
//	)
//
// This works similarly to setting COSIGN_REPOSITORY with cosign.
//
// # Read-Only Managers
//
// For consumers that only need to read status (not write), use NewReadOnly
// with WithExpectedIdentity to specify which signing identity to verify:
//
//	mgr, err := statusmanager.NewReadOnly[MyDetails](ctx, "my-reconciler",
//	    statusmanager.WithExpectedIdentity("producer@project.iam.gserviceaccount.com"),
//	)
//
// # Authentication
//
// The manager uses Google Cloud service account credentials for both:
//   - Obtaining ID tokens for Fulcio keyless signing
//   - Registry authentication (when using WithRemoteOptions with google.Keychain)
//
// Example with registry authentication:
//
//	mgr, err := statusmanager.New[MyDetails](ctx, "my-reconciler",
//	    statusmanager.WithRemoteOptions(remote.WithAuthFromKeychain(google.Keychain)),
//	)
//
// # Thread Safety
//
// Manager instances are safe for concurrent use. Each Session should be used
// by a single goroutine, but multiple sessions can be created from the same
// Manager concurrently.
package statusmanager
