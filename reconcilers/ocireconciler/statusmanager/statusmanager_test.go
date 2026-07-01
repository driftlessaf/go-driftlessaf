//go:build withauth

/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	statusmanagertesting "chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager/testing"
	"github.com/google/go-containerregistry/pkg/name"
	crregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"
)

// setupTestRegistry creates a local test registry server.
func setupTestRegistry(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(crregistry.New(crregistry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)

	// Extract the host from the server URL (strip http:// prefix)
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestStatusManagerSignAndVerify tests the full lifecycle of signing and verifying
// attestations using a real Sigstore infrastructure and local test registry.
func TestStatusManagerSignAndVerify(t *testing.T) {
	ctx := context.Background()

	// Set up local test registry
	registryHost := setupTestRegistry(t)
	t.Logf("Using test registry: %s", registryHost)

	// Create a writable manager for signing with explicit identity
	writer, err := statusmanagertesting.New[TestStatus](ctx, t, "test-reconciler",
		statusmanager.WithRepositoryOverride(registryHost+"/test-repo"),
	)
	require.NoError(t, err, "failed to create writable manager")

	// Create a test digest reference
	digest, err := name.NewDigest("example.com/foo@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.NoError(t, err, "failed to create digest")

	// Create a session
	session := writer.NewSession(digest)

	// Check observed state before writing - should be nil
	observedBefore, err := session.ObservedState(ctx)
	require.NoError(t, err, "failed to check observed state before write")
	require.Nil(t, observedBefore, "expected no status before write")

	t.Log("Confirmed no existing status before write")

	// Write status
	testStatus := &statusmanager.Status[TestStatus]{
		Details: TestStatus{
			Message:   "test reconciliation complete",
			Timestamp: "2025-12-30T00:00:00Z",
		},
	}

	err = session.SetActualState(ctx, testStatus)
	require.NoError(t, err, "failed to write status")

	t.Log("Successfully wrote attestation to test registry")

	// Read back the status using the same writer manager
	observedAfter, err := session.ObservedState(ctx)
	require.NoError(t, err, "failed to read status after write")
	require.NotNil(t, observedAfter, "expected status to be present after write")

	// Verify the status matches
	require.Equal(t, testStatus.Details.Message, observedAfter.Details.Message)
	require.Equal(t, testStatus.Details.Timestamp, observedAfter.Details.Timestamp)
	require.Equal(t, digest.DigestStr(), observedAfter.ObservedGeneration)

	t.Log("Successfully verified attestation using the same manager")

	// Write a different status to test updates
	updatedStatus := &statusmanager.Status[TestStatus]{
		Details: TestStatus{
			Message:   "updated reconciliation complete",
			Timestamp: "2025-12-30T01:00:00Z",
		},
	}

	err = session.SetActualState(ctx, updatedStatus)
	require.NoError(t, err, "failed to write updated status")

	t.Log("Successfully wrote updated attestation")

	// Read back the updated status
	observedUpdated, err := session.ObservedState(ctx)
	require.NoError(t, err, "failed to read updated status")
	require.NotNil(t, observedUpdated, "expected updated status to be present")

	// Verify the updated status matches (should be different from the first)
	require.Equal(t, updatedStatus.Details.Message, observedUpdated.Details.Message)
	require.Equal(t, updatedStatus.Details.Timestamp, observedUpdated.Details.Timestamp)
	require.Equal(t, digest.DigestStr(), observedUpdated.ObservedGeneration)

	// Verify it's actually different from the first status
	require.NotEqual(t, testStatus.Details.Message, observedUpdated.Details.Message)
	require.NotEqual(t, testStatus.Details.Timestamp, observedUpdated.Details.Timestamp)

	t.Log("Successfully verified updated attestation is different from the original")

	// Test read-only manager: Create a read-only manager with the same identity
	readOnlyManager, err := statusmanagertesting.NewReadOnly[TestStatus](ctx, t, "test-reconciler",
		statusmanager.WithRepositoryOverride(registryHost+"/test-repo"),
	)
	require.NoError(t, err, "failed to create read-only manager")

	readOnlySession := readOnlyManager.NewSession(digest)

	// Read-only manager should be able to read the status
	observedReadOnly, err := readOnlySession.ObservedState(ctx)
	require.NoError(t, err, "failed to read status with read-only manager")
	require.NotNil(t, observedReadOnly, "expected read-only manager to read status")
	require.Equal(t, updatedStatus.Details.Message, observedReadOnly.Details.Message)

	t.Log("Read-only manager successfully verified existing attestation")

	// Attempt to write with read-only manager should fail
	attemptedWrite := &statusmanager.Status[TestStatus]{
		Details: TestStatus{
			Message: "should fail",
		},
	}
	err = readOnlySession.SetActualState(ctx, attemptedWrite)
	require.Error(t, err, "expected write to fail on read-only manager")
	require.Contains(t, err.Error(), "read-only", "error should indicate read-only restriction")

	t.Log("Read-only manager correctly rejected write attempt")
}

// TestStatus is a test status structure.
type TestStatus struct {
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// TestStatusManagerWithoutRepositoryOverride tests that the manager works without
// WithRepositoryOverride by deriving the attestation repository from the digest.
func TestStatusManagerWithoutRepositoryOverride(t *testing.T) {
	ctx := context.Background()

	registryHost := setupTestRegistry(t)
	t.Logf("Using test registry: %s", registryHost)

	// Create manager WITHOUT WithRepositoryOverride
	manager, err := statusmanagertesting.New[TestStatus](ctx, t, "no-override-test")
	require.NoError(t, err, "failed to create manager without repository override")

	// Use a digest that references the test registry
	// Without WithRepositoryOverride, attestations will be stored in the same repo as the digest
	digest, err := name.NewDigest(fmt.Sprintf("%s/test-repo@sha256:4444444444444444444444444444444444444444444444444444444444444444", registryHost))
	require.NoError(t, err)

	session := manager.NewSession(digest)

	// Write a status
	testStatus := &statusmanager.Status[TestStatus]{
		Details: TestStatus{
			Message:   "no override test",
			Timestamp: "2025-12-30T02:00:00Z",
		},
	}

	err = session.SetActualState(ctx, testStatus)
	require.NoError(t, err, "failed to write status without repository override")

	t.Log("Successfully wrote attestation without repository override")

	// Read back the status
	observed, err := session.ObservedState(ctx)
	require.NoError(t, err, "failed to read status without repository override")
	require.NotNil(t, observed, "expected status to be present")
	require.Equal(t, testStatus.Details.Message, observed.Details.Message)
	require.Equal(t, testStatus.Details.Timestamp, observed.Details.Timestamp)

	t.Log("Successfully verified attestation stored in digest's repository")

	// Verify that the same digest hash in a different repository doesn't share the status
	// Create a digest with the same hash but different repository
	differentRepoDigest, err := name.NewDigest(fmt.Sprintf("%s/different-repo@sha256:4444444444444444444444444444444444444444444444444444444444444444", registryHost))
	require.NoError(t, err)

	differentRepoSession := manager.NewSession(differentRepoDigest)

	// This should return nil because the attestation is scoped to the repository
	observedDifferentRepo, err := differentRepoSession.ObservedState(ctx)
	require.NoError(t, err, "failed to check status in different repository")
	require.Nil(t, observedDifferentRepo, "expected no status in different repository with same digest hash")

	t.Log("Verified that same digest hash in different repository has no shared status")
}

// TestStatusManagerLargePayload demonstrates that a large status payload now
// signs and round-trips successfully through Rekor.
//
// The previous Rekor v1 hashedrekord path uploaded the entire DSSE payload to
// Rekor, whose reverse proxy caps requests at ~150 MB. To avoid 502s the manager
// rejected any status over StatusJSONSizeLimit (~88 MB) up front, and a now-deleted
// TestStatusManagerSizeLimit asserted that rejection on a 100 MB payload.
//
// Rekor v2 hashedrekord bundles upload only the artifact hash to Rekor; the full
// bundle lives in the OCI registry only. So a 200 MB payload, well past anything
// the v1 path could carry, now signs, stores, and reads back intact.
func TestStatusManagerLargePayload(t *testing.T) {
	ctx := context.Background()

	registryHost := setupTestRegistry(t)
	t.Logf("Using test registry: %s", registryHost)

	manager, err := statusmanagertesting.New[LargeTestStatus](ctx, t, "large-payload-test",
		statusmanager.WithRepositoryOverride(registryHost+"/large-payload-repo"),
	)
	require.NoError(t, err, "failed to create manager")

	digest, err := name.NewDigest("example.com/foo@sha256:3333333333333333333333333333333333333333333333333333333333333333")
	require.NoError(t, err, "failed to create digest")

	session := manager.NewSession(digest)

	// 200 MB is well past both the old StatusJSONSizeLimit (~88 MB) and the ~150 MB
	// Rekor v1 proxy cap (which 502'd once base64/DSSE-wrapped), so it exercises
	// exactly the case the fix targets.
	largeData := strings.Repeat("x", 200*1024*1024)
	status := &statusmanager.Status[LargeTestStatus]{
		Details: LargeTestStatus{Data: largeData},
	}

	err = session.SetActualState(ctx, status)
	require.NoError(t, err, "failed to write large status")

	t.Logf("Successfully wrote a %d MB status", len(largeData)/(1024*1024))

	observed, err := session.ObservedState(ctx)
	require.NoError(t, err, "failed to read back large status")
	require.NotNil(t, observed, "expected large status to be present after write")

	// Compare lengths and identity directly rather than via require.Equal on the strings,
	// which would render a multi-hundred-MB diff on mismatch.
	require.Equal(t, len(largeData), len(observed.Details.Data), "round-tripped payload size mismatch")
	require.True(t, observed.Details.Data == largeData, "round-tripped payload contents mismatch")
	require.Equal(t, digest.DigestStr(), observed.ObservedGeneration)

	t.Log("Successfully read back the large status intact")
}

// LargeTestStatus carries a large payload to exercise the Rekor v2 bundle path.
type LargeTestStatus struct {
	Data string `json:"data"`
}
