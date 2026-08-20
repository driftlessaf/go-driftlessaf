//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs_test

import (
	"os"
	"testing"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/store/blob/blobtest"
	"chainguard.dev/driftlessaf/store/blob/gcs"
)

// TestGCS_Conformance drives the real GCS backend through the same contract
// suite Mem runs, so the two cannot diverge. It needs application default
// credentials and a writable bucket named in BLOB_TEST_BUCKET.
//
//	go test -tags withauth -run TestGCS_Conformance ./store/blob/gcs
func TestGCS_Conformance(t *testing.T) {
	bucketName := os.Getenv("BLOB_TEST_BUCKET")
	if bucketName == "" {
		t.Skip("set BLOB_TEST_BUCKET to a writable GCS bucket to run the GCS conformance suite")
	}

	client, err := storage.NewClient(t.Context())
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	bucket := client.Bucket(bucketName)
	blobtest.RunConformance(t, func() blobtest.Store { return gcs.New(bucket) })
}
