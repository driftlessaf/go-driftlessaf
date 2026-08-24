//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager_test

import (
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager"
	statusmanagertesting "chainguard.dev/driftlessaf/reconcilers/ocireconciler/statusmanager/testing"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	"github.com/stretchr/testify/require"
)

type coexistenceScanStatus struct {
	Result string `json:"result"`
}

func (coexistenceScanStatus) PredicateType() string {
	return "https://status.test/statusmanager/coexistence/scan"
}

type coexistenceSBOMStatus struct {
	Packages []string `json:"packages"`
}

func (coexistenceSBOMStatus) PredicateType() string {
	return "https://status.test/statusmanager/coexistence/sbom"
}

func TestStatusManagersWithDistinctPredicatesCoexist(t *testing.T) {
	ctx := t.Context()
	registryHost := setupTestRegistry(t)
	repository := registryHost + "/predicate-coexistence-repo"

	scanManager, err := statusmanagertesting.New[coexistenceScanStatus](ctx, t,
		statusmanager.WithRepositoryOverride(repository),
	)
	require.NoError(t, err, "creating scan status manager")

	sbomManager, err := statusmanagertesting.New[coexistenceSBOMStatus](ctx, t,
		statusmanager.WithRepositoryOverride(repository),
	)
	require.NoError(t, err, "creating SBOM status manager")

	digest, err := name.NewDigest("example.com/subject@sha256:2222222222222222222222222222222222222222222222222222222222222222")
	require.NoError(t, err, "creating subject digest")
	subject, err := name.NewDigest(repository + "@" + digest.DigestStr())
	require.NoError(t, err, "creating overridden subject digest")

	referrerCounts := func() map[string]int {
		t.Helper()

		idx, err := remote.Referrers(subject)
		require.NoError(t, err, "listing subject referrers")
		manifest, err := idx.IndexManifest()
		require.NoError(t, err, "reading subject referrers index")

		counts := make(map[string]int, len(manifest.Manifests))
		for _, descriptor := range manifest.Manifests {
			referrer, err := remote.Get(subject.Context().Digest(descriptor.Digest.String()))
			require.NoError(t, err, "reading referrer manifest")
			var referrerManifest struct {
				Annotations map[string]string `json:"annotations"`
			}
			require.NoError(t, json.Unmarshal(referrer.Manifest, &referrerManifest), "decoding referrer manifest")
			predicateType, ok := referrerManifest.Annotations[ociremote.BundlePredicateType]
			require.True(t, ok, "referrer manifest must identify its predicate type")
			counts[predicateType]++
		}
		return counts
	}

	scanSession := scanManager.NewSession(digest)
	sbomSession := sbomManager.NewSession(digest)
	initialScan := &statusmanager.Status[coexistenceScanStatus]{
		Details: coexistenceScanStatus{Result: "clean"},
	}
	initialSBOM := &statusmanager.Status[coexistenceSBOMStatus]{
		Details: coexistenceSBOMStatus{Packages: []string{"busybox", "openssl"}},
	}

	require.NoError(t, scanSession.SetActualState(ctx, initialScan), "writing scan status")
	require.NoError(t, sbomSession.SetActualState(ctx, initialSBOM), "writing SBOM status")
	require.Equal(t, map[string]int{
		(coexistenceScanStatus{}).PredicateType(): 1,
		(coexistenceSBOMStatus{}).PredicateType(): 1,
	}, referrerCounts(), "distinct predicates should coexist on the same subject")

	observedScan, err := scanSession.ObservedState(ctx)
	require.NoError(t, err, "reading scan status")
	require.NotNil(t, observedScan, "scan status should remain readable")
	require.Equal(t, initialScan.Details, observedScan.Details)

	observedSBOM, err := sbomSession.ObservedState(ctx)
	require.NoError(t, err, "reading SBOM status")
	require.NotNil(t, observedSBOM, "SBOM status should remain readable")
	require.Equal(t, initialSBOM.Details, observedSBOM.Details)

	updatedScan := &statusmanager.Status[coexistenceScanStatus]{
		Details: coexistenceScanStatus{Result: "findings resolved"},
	}
	require.NoError(t, scanSession.SetActualState(ctx, updatedScan), "updating scan status")

	observedScan, err = scanSession.ObservedState(ctx)
	require.NoError(t, err, "reading updated scan status")
	require.NotNil(t, observedScan, "updated scan status should remain readable")
	require.Equal(t, updatedScan.Details, observedScan.Details)

	observedSBOM, err = sbomSession.ObservedState(ctx)
	require.NoError(t, err, "reading SBOM status after scan update")
	require.NotNil(t, observedSBOM, "SBOM status should survive an independent scan update")
	require.Equal(t, initialSBOM.Details, observedSBOM.Details)

	require.Equal(t, map[string]int{
		(coexistenceScanStatus{}).PredicateType(): 1,
		(coexistenceSBOMStatus{}).PredicateType(): 1,
	}, referrerCounts(), "updates should converge to one referrer per predicate")
}
