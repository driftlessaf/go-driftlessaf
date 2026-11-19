/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blob_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/store/blob"
	"chainguard.dev/driftlessaf/store/blob/blobtest"
)

// TestMem_Conformance drives the in-memory backend through the shared contract
// suite — the same suite the GCS backend runs under withauth — so Mem cannot
// drift from the documented behavior.
func TestMem_Conformance(t *testing.T) {
	blobtest.RunConformance(t, func() blobtest.Store { return blob.NewMem() })
}

func TestMem_VersionConformance(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func() *blob.Mem
	}{
		{name: "wall clock", new: blob.NewMem},
		{name: "injected clock", new: func() *blob.Mem { return blob.NewMemWithClock(time.Now) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			blobtest.RunVersionConformance(t, func() blobtest.VersionStore { return tt.new() })
		})
	}
}

// TestMem_VersionCreated pins that each generation's creation time is the
// injected clock's time when the write that created it succeeded.
func TestMem_VersionCreated(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, time.November, 19, 20, 0, 0, 0, time.UTC)
	store := blob.NewMemWithClock(func() time.Time { return now })

	gen1, err := store.Put(ctx, "k", []byte("first"), blob.Cond{DoesNotExist: true})
	if err != nil {
		t.Fatalf("Put(create): %v", err)
	}
	created1 := now

	now = now.Add(time.Minute)
	if _, err := store.Put(ctx, "k", []byte("retry"), blob.Cond{DoesNotExist: true}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(create, existing): got %v, want ErrPreconditionFailed", err)
	}
	got, ok, err := store.GetVersion(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("GetVersion: ok=%t err=%v", ok, err)
	}
	if want := (blob.Version{Data: []byte("first"), Gen: gen1, Created: created1}); !bytes.Equal(got.Data, want.Data) || got.Gen != want.Gen || !got.Created.Equal(want.Created) {
		t.Errorf("GetVersion after rejected retry: got %+v, want %+v", got, want)
	}

	now = now.Add(time.Minute)
	gen2, err := store.Put(ctx, "k", []byte("second"), blob.Cond{GenerationMatch: gen1})
	if err != nil {
		t.Fatalf("Put(match gen): %v", err)
	}
	got, ok, err = store.GetVersion(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("GetVersion: ok=%t err=%v", ok, err)
	}
	if want := (blob.Version{Data: []byte("second"), Gen: gen2, Created: now}); !bytes.Equal(got.Data, want.Data) || got.Gen != want.Gen || !got.Created.Equal(want.Created) {
		t.Errorf("GetVersion after overwrite: got %+v, want %+v", got, want)
	}
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

	v, ok, err := store.GetVersion(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("GetVersion: ok=%t err=%v", ok, err)
	}
	v.Data[0] = 'Z' // Mutate the slice that GetVersion returned.
	if again, _, _, _ := store.Get(ctx, "k"); !bytes.Equal(again, []byte("original")) {
		t.Errorf("stored body was mutated through GetVersion's slice: got %q", again)
	}
}
