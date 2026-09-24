//go:build withauth

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/gcsstore"
	"chainguard.dev/driftlessaf/agents/note/notetest"
	"chainguard.dev/driftlessaf/store/blob/gcs"
)

// TestGCS_Conformance drives the store against a real bucket through the same
// contract suite it runs over blob.Mem, so the in-memory backend cannot certify
// behavior GCS does not have — object-name handling and prefix-scan paging are
// where the two could plausibly differ.
//
// It needs application default credentials and a writable bucket named in
// NOTE_TEST_BUCKET:
//
//	go test -tags withauth -run TestGCS_Conformance ./agents/note/gcsstore
//
// The suite leaves test objects in the bucket. Each subtest gets a fresh
// timestamped root to isolate its notes; use a bucket lifecycle rule to clean up
// the objects after the run.
func TestGCS_Conformance(t *testing.T) {
	bucketName := os.Getenv("NOTE_TEST_BUCKET")
	if bucketName == "" {
		t.Skip("set NOTE_TEST_BUCKET to a writable GCS bucket to run the GCS conformance suite")
	}

	client, err := storage.NewClient(t.Context())
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	bucket := client.Bucket(bucketName)
	notetest.RunConformance(t, func() note.Store {
		root := fmt.Sprintf("notetest/%d/", time.Now().UnixNano())
		store, err := gcsstore.New(root, gcs.New(bucket))
		if err != nil {
			t.Fatalf("gcsstore.New(%q): %v", root, err)
		}
		return store
	})
}
