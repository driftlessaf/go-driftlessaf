/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blob_test

import (
	"bytes"
	"testing"

	"chainguard.dev/driftlessaf/store/blob"
	"chainguard.dev/driftlessaf/store/blob/blobtest"
)

// TestMem_Conformance drives the in-memory backend through the shared contract
// suite — the same suite the GCS backend runs under withauth — so Mem cannot
// drift from the documented behavior.
func TestMem_Conformance(t *testing.T) {
	blobtest.RunConformance(t, func() blobtest.Store { return blob.NewMem() })
}

// TestMem_BodyIsolation pins that Mem copies bodies in and out, so a caller
// mutating its slice before or after a write can't alter stored bytes.
func TestMem_BodyIsolation(t *testing.T) {
	ctx := t.Context()
	store := blob.NewMem()

	src := []byte("original")
	if _, err := store.Put(ctx, "k", src, blob.Cond{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src[0] = 'X' // Mutate the caller's slice after the write.

	got, _, ok, err := store.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%t err=%v", ok, err)
	}
	if want := []byte("original"); !bytes.Equal(got, want) {
		t.Errorf("stored body was mutated through caller slice: got %q, want %q", got, want)
	}

	got[0] = 'Y' // Mutate the returned slice.
	if again, _, _, _ := store.Get(ctx, "k"); bytes.Equal(again, got) {
		t.Errorf("stored body was mutated through returned slice: got %q", again)
	}
}
