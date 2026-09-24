/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/notetest"
	"chainguard.dev/driftlessaf/store/blob"
)

// newStore builds a Store, failing the test if construction is rejected.
func newStore(t *testing.T, root string, blobs Blobs, opts ...Option) *Store {
	t.Helper()
	s, err := New(root, blobs, opts...)
	if err != nil {
		t.Fatalf("New(%q): %v", root, err)
	}
	return s
}

// TestConformance drives the store through the same contract suite note.Mem
// runs, over the in-memory blob backend. Each subtest gets a fresh backend, so
// the suite's key-wildcard listings see only its own notes.
func TestConformance(t *testing.T) {
	notetest.RunConformance(t, func() note.Store { return newStore(t, "notes/", blob.NewMem()) })
}

// TestConformance_EmptyRoot pins that a store at the bucket root behaves
// identically — the root is a namespace, not a required path component.
func TestConformance_EmptyRoot(t *testing.T) {
	notetest.RunConformance(t, func() note.Store { return newStore(t, "", blob.NewMem()) })
}

// TestNew_Rejects pins the construction-time gate: a root that is absolute or
// walks upward is refused, matching the prefixing rules the sibling checkpoint
// store applies, and a missing backend is refused rather than deferred to a nil
// dereference on first use.
func TestNew_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  string
		blobs Blobs
	}{
		{"absolute root", "/orgs/example/", blob.NewMem()},
		{"root walks upward", "orgs/../example/", blob.NewMem()},
		{"root is only a parent segment", "..", blob.NewMem()},
		{"no backend", "orgs/example/", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := New(tc.root, tc.blobs); err == nil {
				t.Errorf("New(%q): got %+v, want an error", tc.root, got)
			}
		})
	}
}

// TestNew_RootGetsTrailingSlash pins that a root without a trailing slash gets
// one, so one tenant's root cannot prefix another's: without it a store rooted
// at "org-1" would list "org-10"'s notes too.
func TestNew_RootGetsTrailingSlash(t *testing.T) {
	backend := blob.NewMem()
	unslashed := newStore(t, "org-1", backend)
	if err := unslashed.Put(t.Context(), note.Note{Key: "k", Run: 1, Name: "fix_plan"}, strings.NewReader("mine")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	page, err := backend.List(t.Context(), "", 0, "")
	if err != nil {
		t.Fatalf("blob List: %v", err)
	}
	if len(page.Objects) != 1 {
		t.Fatalf("blob List: got %d objects, want 1", len(page.Objects))
	}
	if want := "org-1/key_k/run_1/name_fix_plan/author_"; page.Objects[0].Name != want {
		t.Errorf("object name: got %q, want %q", page.Objects[0].Name, want)
	}

	// A neighbouring tenant whose root shares the prefix is not visible.
	neighbour := newStore(t, "org-10", backend)
	if err := neighbour.Put(t.Context(), note.Note{Key: "k", Run: 1, Name: "fix_plan"}, strings.NewReader("theirs")); err != nil {
		t.Fatalf("Put(neighbour): %v", err)
	}
	notes, err := unslashed.List(t.Context(), note.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(notes.Notes) != 1 {
		t.Errorf("store at root %q sees %d notes, want only its own", "org-1", len(notes.Notes))
	}
}

// TestPut_RejectsOversizedBody pins the memory ceiling: a body over the limit
// fails the write rather than being read without bound, and nothing is stored.
func TestPut_RejectsOversizedBody(t *testing.T) {
	// Lower the ceiling via the option so the test does not have to allocate the
	// real one, and so no package-level state is mutated.
	const limit = 32
	backend := blob.NewMem()
	store := newStore(t, "notes/", backend, WithMaxNoteSize(limit))
	n := note.Note{Key: "k", Run: 1, Name: "fix_plan", Author: "claude"}

	// At the limit the write lands.
	if err := store.Put(t.Context(), n, strings.NewReader(strings.Repeat("a", limit))); err != nil {
		t.Errorf("Put(at the limit): %v", err)
	}
	// One byte over, it does not.
	err := store.Put(t.Context(), note.Note{Key: "k", Run: 1, Name: "critique"}, strings.NewReader(strings.Repeat("a", limit+1)))
	if err == nil {
		t.Fatal("Put(over the limit): got nil error, want a size error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Put(over the limit): got %v, want a size error", err)
	}
	if _, _, getErr := store.Get(t.Context(), "k", 1, "critique", ""); !errors.Is(getErr, note.ErrNotExist) {
		t.Errorf("an oversized note was stored: Get returned %v, want ErrNotExist", getErr)
	}
}

func TestPut_MaxInt64Limit(t *testing.T) {
	for _, limit := range []int64{math.MaxInt64 - 1, math.MaxInt64} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			backend := blob.NewMem()
			n := note.Note{Key: "k", Run: 1, Name: "fix_plan"}
			if err := newStore(t, "notes/", backend).Put(t.Context(), n, strings.NewReader(rand.Text())); err != nil {
				t.Fatalf("Put(original): %v", err)
			}

			store := newStore(t, "notes/", backend, WithMaxNoteSize(limit))
			want := rand.Text()
			if err := store.Put(t.Context(), n, strings.NewReader(want)); err != nil {
				t.Fatalf("Put(replacement): %v", err)
			}
			_, body, err := store.Get(t.Context(), n.Key, n.Run, n.Name, n.Author)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != want {
				t.Errorf("body: got %q, want %q", got, want)
			}
		})
	}
}

// TestPut_PropagatesBodyReadError pins that a failing body reader fails the Put
// rather than storing a truncated note.
func TestPut_PropagatesBodyReadError(t *testing.T) {
	backend := blob.NewMem()
	store := newStore(t, "notes/", backend)

	wantErr := errors.New("body went away")
	err := store.Put(t.Context(), note.Note{Key: "k", Run: 1, Name: "fix_plan"}, io.MultiReader(
		strings.NewReader("the start of a plan"),
		&failingReader{err: wantErr},
	))
	if !errors.Is(err, wantErr) {
		t.Errorf("Put: got %v, want it to wrap %v", err, wantErr)
	}
	if page, _ := backend.List(t.Context(), "", 0, ""); len(page.Objects) != 0 {
		t.Errorf("a truncated note was stored: got %+v, want none", page.Objects)
	}
}

// failingReader fails every read, standing in for a body whose source dies
// part-way through.
type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestList_SkipsForeignObjects pins that a root may be shared with other data:
// an object that is not shaped like a note object is skipped, not fatal. This is
// the shape skillchain already has, where a version's notes sit beside its
// hardened tree and manifests — erroring here would break the fan-in for
// everything under the root, permanently.
func TestList_SkipsForeignObjects(t *testing.T) {
	backend := blob.NewMem()
	store := newStore(t, "notes/", backend)

	want := note.Note{Key: "sha", Run: 1, Name: "harden/fix_plan", Author: "claude"}
	if err := store.Put(t.Context(), want, strings.NewReader("the plan")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, foreign := range []string{
		"notes/SKILL.md",                                      // a sibling file
		"notes/hardened/tree/file.txt",                        // a sibling subtree
		"notes/key_k/run_1/name_n",                            // too few segments
		"notes/key_k/run_1/name_fix_plan/author_claude/extra", // too many segments
		"notes/k/run_1/name_fix_plan/author_claude",           // no key role prefix
		"notes/key_k/1/name_fix_plan/author_claude",           // no run role prefix
		"notes/key_k/run_1/fix_plan/author_claude",            // no name role prefix
		"notes/key_k/run_1/name_fix_plan/claude",              // no author role prefix
		"notes/key_%zz/assets/name_fix_plan/author_claude",    // invalid key, no run role prefix
		"notes/key_k/run_assets/tree/file.txt",                // invalid run, no name or author role prefix
		"notes/key_k/run_1/name_%zz/file.txt",                 // invalid name, no author role prefix
	} {
		if _, err := backend.Put(t.Context(), foreign, []byte("x"), blob.Cond{}); err != nil {
			t.Fatalf("seeding %q: %v", foreign, err)
		}
	}

	page, err := store.List(t.Context(), note.Filter{})
	if err != nil {
		t.Fatalf("List with foreign objects present: %v", err)
	}
	if diff := cmp.Diff([]note.Note{want}, page.Notes); diff != "" {
		t.Errorf("List (-want, +got):\n%s", diff)
	}
}

// TestList_SkipsANestedStore pins the same property for the case two note stores
// share a bucket with one root beneath the other: the outer store's listing must
// not choke on the inner store's objects.
func TestList_SkipsANestedStore(t *testing.T) {
	backend := blob.NewMem()
	outer := newStore(t, "a/", backend)
	inner := newStore(t, "a/b/", backend)

	outerNote := note.Note{Key: "k", Run: 1, Name: "n"}
	if err := outer.Put(t.Context(), outerNote, strings.NewReader("outer")); err != nil {
		t.Fatalf("outer Put: %v", err)
	}
	if err := inner.Put(t.Context(), note.Note{Key: "k", Run: 1, Name: "n"}, strings.NewReader("inner")); err != nil {
		t.Fatalf("inner Put: %v", err)
	}

	page, err := outer.List(t.Context(), note.Filter{})
	if err != nil {
		t.Fatalf("outer List: %v", err)
	}
	if diff := cmp.Diff([]note.Note{outerNote}, page.Notes); diff != "" {
		t.Errorf("outer List (-want, +got):\n%s", diff)
	}
}

// TestList_RejectsNonCanonicalNote pins the other half of the split: a name that
// IS shaped like a note but is not the canonical spelling of its coordinates
// fails the listing. Two object names decoding to one note is the aliasing the
// encoding exists to prevent, so this must never be skipped the way a foreign
// object is — skipping would hide exactly the bug the canonical check catches.
func TestList_RejectsNonCanonicalNote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objName string
	}{
		{"run is not canonical", "notes/key_k/run_01/name_fix_plan/author_claude"},
		{"escape uses lowercase hex", "notes/key_a%2fb/run_1/name_fix_plan/author_claude"},
		{"value escaped when it need not be", "notes/key_%6B/run_1/name_fix_plan/author_claude"},
		{"run is not a number", "notes/key_k/run_one/name_fix_plan/author_claude"},
		{"run below one", "notes/key_k/run_0/name_fix_plan/author_claude"},
		{"malformed escape", "notes/key_a%zz/run_1/name_fix_plan/author_claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := blob.NewMem()
			if _, err := backend.Put(t.Context(), tc.objName, []byte("x"), blob.Cond{}); err != nil {
				t.Fatalf("seeding %q: %v", tc.objName, err)
			}
			_, err := newStore(t, "notes/", backend).List(t.Context(), note.Filter{})
			if err == nil {
				t.Errorf("List over %q: got nil error, want a rejection", tc.objName)
			}
			if errors.Is(err, errNotANote) {
				t.Errorf("List over %q: got errNotANote (skippable), want a fatal rejection", tc.objName)
			}
		})
	}
}

// TestList_ShortPageStillPages pins the paging contract this backend makes
// visible: a prefix scan bounds objects, not matches, so a page can be empty
// while the cursor is non-empty. A caller that stopped on a short page would
// miss the matching note at the end of the scan.
func TestList_ShortPageStillPages(t *testing.T) {
	backend := blob.NewMem()
	store := newStore(t, "notes/", backend)

	// Ten notes under one key, one of them by the author being filtered for, and
	// ordered so it sorts last: every earlier page is all non-matches.
	for i := range 10 {
		author := "claude"
		if i == 9 {
			author = "zephyr"
		}
		n := note.Note{Key: "K", Run: 1, Name: fmt.Sprintf("step_%02d", i), Author: author}
		if err := store.Put(t.Context(), n, strings.NewReader("body")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	zephyr := "zephyr"
	filter := note.Filter{Key: "K", Author: &zephyr, Limit: 2}
	var got []note.Note
	sawShortPage := false
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := store.List(t.Context(), filter)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Notes) == 0 && page.Cursor != "" {
			sawShortPage = true
		}
		got = append(got, page.Notes...)
		if page.Cursor == "" {
			break
		}
		filter.Cursor = page.Cursor
	}

	if !sawShortPage {
		t.Errorf("saw an empty page with a live cursor: got = %t, want = true", sawShortPage)
	}
	if len(got) != 1 || got[0].Author != "zephyr" {
		t.Errorf("paged result: got %+v, want the single zephyr note", got)
	}
}

// TestList_PrefixNarrowsTheScan pins that a filter's leading coordinates are
// pushed into the prefix rather than scanned and discarded: with a key, run and
// name fixed, the scan must not touch the other key's objects.
func TestList_PrefixNarrowsTheScan(t *testing.T) {
	backend := &countingBlobs{Blobs: blob.NewMem()}
	store := newStore(t, "notes/", backend)

	for _, n := range []note.Note{
		{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"},
		{Key: "K", Run: 1, Name: "fix_plan", Author: "gemini"},
		{Key: "K", Run: 2, Name: "fix_plan", Author: "claude"},
		{Key: "other", Run: 1, Name: "fix_plan", Author: "claude"},
	} {
		if err := store.Put(t.Context(), n, strings.NewReader("body")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	run1 := 1
	page, err := store.List(t.Context(), note.Filter{Key: "K", Run: &run1, Name: "fix_plan"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Notes) != 2 {
		t.Errorf("List: got %d notes, want 2", len(page.Notes))
	}
	if want := "notes/key_K/run_1/name_fix_plan/"; backend.lastPrefix != want {
		t.Errorf("scan prefix: got %q, want %q", backend.lastPrefix, want)
	}
	if backend.scanned != 2 {
		t.Errorf("objects scanned: got %d, want 2 — the prefix should exclude the other key and run", backend.scanned)
	}
}

// countingBlobs records what a List actually scanned, so a test can assert the
// prefix did the narrowing rather than the residual match.
type countingBlobs struct {
	Blobs
	lastPrefix string
	scanned    int
}

func (c *countingBlobs) List(ctx context.Context, prefix string, limit int, cursor string) (blob.Page, error) {
	page, err := c.Blobs.List(ctx, prefix, limit, cursor)
	c.lastPrefix = prefix
	c.scanned += len(page.Objects)
	return page, err
}
