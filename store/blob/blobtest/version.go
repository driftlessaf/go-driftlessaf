/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blobtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/store/blob"
)

// VersionStore is the method set that [RunVersionConformance] drives: the
// [Store] methods plus GetVersion. Both *blob.Mem and the GCS backend satisfy
// it implicitly.
type VersionStore interface {
	Store
	GetVersion(ctx context.Context, name string) (blob.Version, bool, error)
}

// RunVersionConformance drives newStore() through the GetVersion contract.
// GetVersion returns the bytes, generation and creation time of one
// generation. A generation's creation time never changes, a rejected
// conditional write leaves the current version as it was, and a successful
// write creates a new generation with its own creation time. Names are
// namespaced under a unique prefix so a shared bucket is safe, and the suite
// deletes what it creates.
func RunVersionConformance(t *testing.T, newStore func() VersionStore) {
	t.Helper()
	ctx := t.Context()
	store := newStore()

	root := fmt.Sprintf("blobtest/%d/", time.Now().UnixNano())
	name := root + "version"

	if v, ok, err := store.GetVersion(ctx, name); err != nil || ok {
		t.Fatalf("GetVersion(absent): got %+v ok=%t err=%v, want ok=false err=nil", v, ok, err)
	}

	body := []byte("the first generation")
	gen1, err := store.Put(ctx, name, body, blob.Cond{DoesNotExist: true})
	if err != nil {
		t.Fatalf("Put(create): %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.WithoutCancel(ctx), name, 0) })

	first := getVersion(t, store, name)
	if first.Created.IsZero() {
		t.Fatalf("GetVersion: got zero creation time, want the time storage created generation %d", gen1)
	}
	if want := (blob.Version{Data: body, Gen: gen1, Created: first.Created}); !equalVersion(first, want) {
		t.Errorf("GetVersion: got %+v, want %+v", first, want)
	}

	// Get reads the same generation.
	if data, gen, ok, err := store.Get(ctx, name); err != nil || !ok || !bytes.Equal(data, first.Data) || gen != first.Gen {
		t.Errorf("Get: got %q gen=%d ok=%t err=%v, want %q gen=%d ok=true", data, gen, ok, err, first.Data, first.Gen)
	}

	// Reading the same generation again returns the same creation time.
	if again := getVersion(t, store, name); !equalVersion(again, first) {
		t.Errorf("GetVersion(again): got %+v, want %+v", again, first)
	}

	// Rejected conditional writes leave the version as it was.
	if _, err := store.Put(ctx, name, []byte("again"), blob.Cond{DoesNotExist: true}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(create, existing): got %v, want ErrPreconditionFailed", err)
	}
	if _, err := store.Put(ctx, name, []byte("stale"), blob.Cond{GenerationMatch: gen1 + 1000}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(wrong gen): got %v, want ErrPreconditionFailed", err)
	}
	if got := getVersion(t, store, name); !equalVersion(got, first) {
		t.Errorf("GetVersion after rejected writes: got %+v, want %+v", got, first)
	}

	// A successful write creates a new generation with its own creation time.
	updated := []byte("the second generation")
	gen2, err := store.Put(ctx, name, updated, blob.Cond{GenerationMatch: gen1})
	if err != nil {
		t.Fatalf("Put(match gen): %v", err)
	}
	second := getVersion(t, store, name)
	if second.Created.Before(first.Created) {
		t.Errorf("GetVersion after overwrite: created %v, want no earlier than %v", second.Created, first.Created)
	}
	if want := (blob.Version{Data: updated, Gen: gen2, Created: second.Created}); !equalVersion(second, want) {
		t.Errorf("GetVersion after overwrite: got %+v, want %+v", second, want)
	}

	// An empty body is a version with no bytes.
	empty := root + "empty"
	emptyGen, err := store.Put(ctx, empty, []byte{}, blob.Cond{DoesNotExist: true})
	if err != nil {
		t.Fatalf("Put(empty): %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.WithoutCancel(ctx), empty, 0) })
	if got := getVersion(t, store, empty); len(got.Data) != 0 || got.Gen != emptyGen || got.Created.IsZero() {
		t.Errorf("GetVersion(empty): got %+v, want no bytes, gen %d and a creation time", got, emptyGen)
	}

	// A deleted object has no version.
	if err := store.Delete(ctx, name, gen2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if v, ok, err := store.GetVersion(ctx, name); err != nil || ok {
		t.Errorf("GetVersion after Delete: got %+v ok=%t err=%v, want ok=false err=nil", v, ok, err)
	}
}

// getVersion returns name's current version and fails the test when it is
// absent or the read fails.
func getVersion(t *testing.T, store VersionStore, name string) blob.Version {
	t.Helper()
	v, ok, err := store.GetVersion(t.Context(), name)
	if err != nil || !ok {
		t.Fatalf("GetVersion(%q): got ok=%t err=%v, want ok=true err=nil", name, ok, err)
	}
	return v
}

// equalVersion reports whether two versions have the same bytes, generation
// and creation instant.
func equalVersion(a, b blob.Version) bool {
	return bytes.Equal(a.Data, b.Data) && a.Gen == b.Gen && a.Created.Equal(b.Created)
}
