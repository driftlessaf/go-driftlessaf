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

// Store is the blob backend method set the conformance suite drives. It is
// declared here — by the consumer — rather than exported by the blob package,
// following "accept interfaces, return structs": both *blob.Mem and the GCS
// backend satisfy it implicitly.
type Store interface {
	Put(ctx context.Context, name string, data []byte, cond blob.Cond) (blob.Gen, error)
	Get(ctx context.Context, name string) ([]byte, blob.Gen, bool, error)
	Delete(ctx context.Context, name string, ifGen blob.Gen) error
	List(ctx context.Context, prefix string, limit int, cursor string) (blob.Page, error)
}

// RunConformance drives newStore() through the blob contract — CAS writes,
// generation-conditional deletes, prefix listing with pagination, and the
// sentinel errors — so a backend proves it matches the documented behavior.
// newStore must return a usable backend; names are namespaced under a unique
// prefix so a shared bucket is safe, and the suite deletes what it creates on
// the happy path.
func RunConformance(t *testing.T, newStore func() Store) {
	t.Helper()
	ctx := t.Context()
	store := newStore()

	root := fmt.Sprintf("blobtest/%d/", time.Now().UnixNano())
	name := root + "notes/run_1/claude/fix_plan"
	body := []byte("the plan")

	// Absent: Get reports ok=false with no error. An unconditional Delete of an
	// absent object is ErrNotExist; a generation-conditional Delete of an absent
	// object is ErrPreconditionFailed (the generation can never match).
	if _, _, ok, err := store.Get(ctx, name); err != nil || ok {
		t.Fatalf("Get(absent): got ok=%t err=%v, want ok=false err=nil", ok, err)
	}
	if err := store.Delete(ctx, name, 0); !errors.Is(err, blob.ErrNotExist) {
		t.Fatalf("Delete(absent, unconditional): got %v, want ErrNotExist", err)
	}
	if err := store.Delete(ctx, name, 5); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Delete(absent, ifGen>0): got %v, want ErrPreconditionFailed", err)
	}

	// Create-only Put lands and returns a positive generation.
	gen1, err := store.Put(ctx, name, body, blob.Cond{DoesNotExist: true})
	if err != nil {
		t.Fatalf("Put(create): %v", err)
	}
	if gen1 <= 0 {
		t.Fatalf("Put(create): got gen %d, want > 0", gen1)
	}

	// Get returns the stored body and the create generation.
	got, gen, ok, err := store.Get(ctx, name)
	if err != nil || !ok {
		t.Fatalf("Get: got ok=%t err=%v, want ok=true err=nil", ok, err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("Get body: got %q, want %q", got, body)
	}
	if gen != gen1 {
		t.Errorf("Get gen: got %d, want %d", gen, gen1)
	}

	// A second create-only Put is rejected: the object already exists.
	if _, err := store.Put(ctx, name, []byte("again"), blob.Cond{DoesNotExist: true}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(create, existing): got %v, want ErrPreconditionFailed", err)
	}

	// A GenerationMatch Put against a wrong (non-zero) generation is rejected.
	if _, err := store.Put(ctx, name, []byte("stale"), blob.Cond{GenerationMatch: gen1 + 1000}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(wrong gen): got %v, want ErrPreconditionFailed", err)
	}

	// A GenerationMatch Put against an absent object is rejected — the generation
	// can never match. (Backends must agree on this sentinel.)
	if _, err := store.Put(ctx, root+"absent-gen", []byte("x"), blob.Cond{GenerationMatch: 42}); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Put(GenerationMatch, absent): got %v, want ErrPreconditionFailed", err)
	}

	// A GenerationMatch Put against the current generation succeeds and advances
	// the generation.
	updated := []byte("the revised plan")
	gen2, err := store.Put(ctx, name, updated, blob.Cond{GenerationMatch: gen1})
	if err != nil {
		t.Fatalf("Put(match gen): %v", err)
	}
	if gen2 <= gen1 {
		t.Errorf("Put(match gen): got gen %d, want > %d", gen2, gen1)
	}
	if got, _, _, err := store.Get(ctx, name); err != nil || !bytes.Equal(got, updated) {
		t.Errorf("Get after overwrite: got %q err=%v, want %q", got, err, updated)
	}

	// An unconditional Put (zero Cond) overwrites the existing object and advances
	// the generation.
	overwritten := []byte("unconditional overwrite")
	gen3, err := store.Put(ctx, name, overwritten, blob.Cond{})
	if err != nil {
		t.Fatalf("Put(unconditional overwrite): %v", err)
	}
	if gen3 <= gen2 {
		t.Errorf("Put(unconditional overwrite): got gen %d, want > %d", gen3, gen2)
	}
	if got, g, _, err := store.Get(ctx, name); err != nil || !bytes.Equal(got, overwritten) || g != gen3 {
		t.Errorf("Get after unconditional overwrite: got %q gen=%d err=%v, want %q gen=%d", got, g, err, overwritten, gen3)
	}

	// List under the unique root returns exactly this object at its current gen;
	// a non-matching prefix returns nothing. Neither is truncated.
	page, err := store.List(ctx, root, 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Name != name || page.Objects[0].Gen != gen3 {
		t.Errorf("List(%q): got %+v, want one entry {%q %d}", root, page.Objects, name, gen3)
	}
	if page.Cursor != "" {
		t.Errorf("List(%q): got cursor %q, want empty (single page)", root, page.Cursor)
	}
	if p, err := store.List(ctx, root+"absent/", 0, ""); err != nil || len(p.Objects) != 0 {
		t.Errorf("List(non-matching prefix): got %+v err=%v, want empty", p.Objects, err)
	}

	// DoesNotExist takes precedence over GenerationMatch: a create-only Put of a
	// fresh name succeeds even with a GenerationMatch that could never match,
	// because the object is absent. (Backends must agree here; the two once
	// diverged on this exact combination.)
	name2 := root + "notes/run_1/gemini/fix_plan"
	if _, err := store.Put(ctx, name2, []byte("g"), blob.Cond{DoesNotExist: true, GenerationMatch: 999}); err != nil {
		t.Fatalf("Put(DoesNotExist precedence over GenerationMatch): got %v, want success", err)
	}
	if err := store.Delete(ctx, name2, 0); err != nil {
		t.Fatalf("Delete(name2 cleanup): %v", err)
	}

	// Delete with a wrong generation is rejected and leaves the object intact.
	if err := store.Delete(ctx, name, gen3+1000); !errors.Is(err, blob.ErrPreconditionFailed) {
		t.Fatalf("Delete(wrong gen): got %v, want ErrPreconditionFailed", err)
	}
	if _, _, ok, _ := store.Get(ctx, name); !ok {
		t.Fatalf("Get after rejected Delete: got ok=false, want ok=true (object must survive)")
	}

	// Delete with the current generation removes the object.
	if err := store.Delete(ctx, name, gen3); err != nil {
		t.Fatalf("Delete(match gen): %v", err)
	}
	if _, _, ok, _ := store.Get(ctx, name); ok {
		t.Fatalf("Get after Delete: got ok=true, want ok=false")
	}

	runPagination(t, store, root)
}

// runPagination pins that List caps at the limit and pages the full result set
// via the cursor exactly once, with no duplicates and a terminating empty cursor.
func runPagination(t *testing.T, store Store, root string) {
	t.Helper()
	ctx := t.Context()

	const total = 5
	prefix := root + "page/"
	for i := range total {
		if _, err := store.Put(ctx, fmt.Sprintf("%sn%d", prefix, i), []byte("x"), blob.Cond{}); err != nil {
			t.Fatalf("Put(page seed): %v", err)
		}
	}
	t.Cleanup(func() {
		for i := range total {
			_ = store.Delete(ctx, fmt.Sprintf("%sn%d", prefix, i), 0)
		}
	})

	seen := make(map[string]int, total)
	pages, cursor := 0, ""
	for {
		page, err := store.List(ctx, prefix, 2, cursor)
		if err != nil {
			t.Fatalf("List(page): %v", err)
		}
		if len(page.Objects) > 2 {
			t.Fatalf("List over limit: got %d objects, want <= 2", len(page.Objects))
		}
		for _, o := range page.Objects {
			seen[o.Name]++
		}
		pages++
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("paged coverage: got %d distinct objects, want %d", len(seen), total)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("object %q returned %d times, want exactly once", name, n)
		}
	}
}
