/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package notetest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/note"
)

// RunConformance drives newStore() through the note.Store contract — the
// Put/Get round-trip, the fail-closed Validate gate, the shared Author scope,
// coordinate values that must not alias on a path-laid-out backend, every
// Filter subset, and paging a listing to completion — so a backend proves it
// matches the documented behavior rather than only its own tests.
//
// newStore is called once per subtest and must return a store over a fresh,
// empty namespace: the suite lists with key-wildcard filters and asserts exact
// result sets, so a store that can see notes from another test — or another run
// against the same bucket — will fail. A durable backend satisfies this by
// generating a unique root prefix per call.
func RunConformance(t *testing.T, newStore func() note.Store) {
	t.Helper()

	for _, tc := range []struct {
		name string
		run  func(*testing.T, note.Store)
	}{
		{"get absent", runGetAbsent},
		{"put get round trip", runRoundTrip},
		{"put rejects an invalid note", runPutRejectsInvalid},
		{"put overwrites the same coordinates", runOverwrite},
		{"empty body", runEmptyBody},
		{"shared author scope", runSharedScope},
		{"delete", runDelete},
		{"delete addresses one note", runDeleteIsExact},
		{"body isolation", runBodyIsolation},
		{"coordinates do not alias", runNoAliasing},
		{"list filters by coordinate subset", runListFilter},
		{"list pages to completion", runListPagination},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newStore())
		})
	}
}

// runGetAbsent pins that absence is reported as note.ErrNotExist rather than an
// empty body or a backend-specific error.
func runGetAbsent(t *testing.T, s note.Store) {
	if _, _, err := s.Get(t.Context(), "nothing", 1, "fix_plan", "claude"); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get(absent): got %v, want note.ErrNotExist", err)
	}
}

// runRoundTrip pins that Get returns both the body Put stored and the
// coordinates it was addressed by, and that a sibling author is a distinct note.
func runRoundTrip(t *testing.T, s note.Store) {
	want := note.Note{Key: "harden:widget", Run: 3, Name: "fix_plan", Author: "claude"}
	const wantBody = "rebuild the lockfile, then re-run the suite"
	put(t, s, want, wantBody)

	got, body := get(t, s, want.Key, want.Run, want.Name, want.Author)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get coordinates (-want, +got):\n%s", diff)
	}
	if body != wantBody {
		t.Errorf("body: got %q, want %q", body, wantBody)
	}

	// Only the stored author exists: a note is addressed by all four coordinates.
	if _, _, err := s.Get(t.Context(), want.Key, want.Run, want.Name, "gemini"); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get(other author): got %v, want note.ErrNotExist", err)
	}
}

// runPutRejectsInvalid pins that Put fails closed on the shared Note.Validate
// gate and stores nothing — the invariant that keeps "what is a storable note"
// defined once instead of per backend.
func runPutRejectsInvalid(t *testing.T, s note.Store) {
	for _, n := range []note.Note{
		{Run: 1, Name: "fix_plan"},            // no Key
		{Key: "k", Run: 1},                    // no Name
		{Key: "k", Name: "fix_plan"},          // Run 0
		{Key: "k", Run: -1, Name: "fix_plan"}, // negative Run
	} {
		if err := s.Put(t.Context(), n, strings.NewReader("body")); err == nil {
			t.Errorf("Put(%+v): got nil error, want a validation error", n)
		}
	}

	page, err := s.List(t.Context(), note.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Notes) != 0 {
		t.Errorf("an invalid note was stored: got %+v, want none", page.Notes)
	}
}

// runOverwrite pins that Put under existing coordinates replaces the body
// rather than erroring or appending a second note.
func runOverwrite(t *testing.T, s note.Store) {
	n := note.Note{Key: "harden:widget", Run: 1, Name: "critique", Author: "claude"}
	put(t, s, n, "the first pass")
	put(t, s, n, "the second pass supersedes it")

	if _, body := get(t, s, n.Key, n.Run, n.Name, n.Author); body != "the second pass supersedes it" {
		t.Errorf("body after overwrite: got %q, want the second pass", body)
	}
	page, err := s.List(t.Context(), note.Filter{Key: n.Key})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Notes) != 1 {
		t.Errorf("overwrite created a second note: got %+v, want one", page.Notes)
	}
}

// runEmptyBody pins that a nil reader and an empty reader both store a note that
// exists with an empty body — a note's coordinates carry meaning even when its
// body does not.
func runEmptyBody(t *testing.T, s note.Store) {
	nilBody := note.Note{Key: "k", Run: 1, Name: "nil_body"}
	if err := s.Put(t.Context(), nilBody, nil); err != nil {
		t.Fatalf("Put(nil body): %v", err)
	}
	put(t, s, note.Note{Key: "k", Run: 1, Name: "empty_body"}, "")

	for _, name := range []string{"nil_body", "empty_body"} {
		if _, body := get(t, s, "k", 1, name, ""); body != "" {
			t.Errorf("%s: got body %q, want empty", name, body)
		}
	}
}

// runSharedScope pins that Author "" is a note in its own right — the shared /
// synthesizer scope — distinct from an authored note with the same other three
// coordinates, selectable exactly with Author: &"" and included by the wildcard.
func runSharedScope(t *testing.T, s note.Store) {
	shared := note.Note{Key: "K", Run: 1, Name: "fix_plan"}
	authored := note.Note{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"}
	put(t, s, shared, "the shared plan")
	put(t, s, authored, "claude's plan")

	if _, body := get(t, s, "K", 1, "fix_plan", ""); body != "the shared plan" {
		t.Errorf("shared note body: got %q, want the shared plan", body)
	}
	if _, body := get(t, s, "K", 1, "fix_plan", "claude"); body != "claude's plan" {
		t.Errorf("authored note body: got %q, want claude's plan", body)
	}

	emptyAuthor := ""
	assertList(t, s, note.Filter{Key: "K", Author: &emptyAuthor}, []note.Note{shared})
	assertList(t, s, note.Filter{Key: "K"}, []note.Note{shared, authored})
}

// runDelete pins the removal contract: an existing note goes away, the absence
// is reported the same way Get reports it, and the coordinates are reusable
// afterwards — a delete leaves no tombstone that would block a re-Put.
func runDelete(t *testing.T, s note.Store) {
	n := note.Note{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"}
	put(t, s, n, "the plan")

	if err := s.Delete(t.Context(), n.Key, n.Run, n.Name, n.Author); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Get(t.Context(), n.Key, n.Run, n.Name, n.Author); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get after Delete: got %v, want note.ErrNotExist", err)
	}
	page, err := s.List(t.Context(), note.Filter{Key: n.Key})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Notes) != 0 {
		t.Errorf("List after Delete: got %+v, want none", page.Notes)
	}

	// Deleting again reports absence rather than succeeding silently, so a
	// caller can tell "I removed it" from "it was already gone".
	if err := s.Delete(t.Context(), n.Key, n.Run, n.Name, n.Author); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Delete(absent): got %v, want note.ErrNotExist", err)
	}
	// Absence for coordinates that never existed is the same signal.
	if err := s.Delete(t.Context(), "never", 1, "harden/fix_plan", "claude"); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Delete(never existed): got %v, want note.ErrNotExist", err)
	}

	// The coordinates are reusable: no tombstone survives the delete.
	put(t, s, n, "the replacement plan")
	if _, body := get(t, s, n.Key, n.Run, n.Name, n.Author); body != "the replacement plan" {
		t.Errorf("Put after Delete: got %q, want the replacement plan", body)
	}
}

// runDeleteIsExact pins that Delete addresses one note and not a group. The
// siblings most at risk are the ones sharing three coordinates: another author's
// note, and the shared Author "" scope, which a path-laid-out backend stores
// under a prefix of the authored names.
func runDeleteIsExact(t *testing.T, s note.Store) {
	var (
		claude   = note.Note{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"}
		gemini   = note.Note{Key: "K", Run: 1, Name: "fix_plan", Author: "gemini"}
		shared   = note.Note{Key: "K", Run: 1, Name: "fix_plan"}
		otherRun = note.Note{Key: "K", Run: 2, Name: "fix_plan", Author: "claude"}
		otherKey = note.Note{Key: "other", Run: 1, Name: "fix_plan", Author: "claude"}
	)
	for _, n := range []note.Note{claude, gemini, shared, otherRun, otherKey} {
		put(t, s, n, "body")
	}

	if err := s.Delete(t.Context(), claude.Key, claude.Run, claude.Name, claude.Author); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, survivor := range []note.Note{gemini, shared, otherRun, otherKey} {
		if _, _, err := s.Get(t.Context(), survivor.Key, survivor.Run, survivor.Name, survivor.Author); err != nil {
			t.Errorf("Get(%+v) after deleting a sibling: %v", survivor, err)
		}
	}

	// Deleting the shared scope must not take the remaining authored note.
	if err := s.Delete(t.Context(), shared.Key, shared.Run, shared.Name, shared.Author); err != nil {
		t.Fatalf("Delete(shared scope): %v", err)
	}
	if _, _, err := s.Get(t.Context(), gemini.Key, gemini.Run, gemini.Name, gemini.Author); err != nil {
		t.Errorf("Get(%+v) after deleting the shared scope: %v", gemini, err)
	}
}

// runBodyIsolation pins that stored bytes are decoupled from the caller's — a
// note is a durable copy, so mutating the source after a Put, or the bytes a Get
// returned, must not reach into the store.
func runBodyIsolation(t *testing.T, s note.Store) {
	n := note.Note{Key: "k", Run: 1, Name: "fix_plan", Author: "claude"}
	src := []byte("original")
	if err := s.Put(t.Context(), n, bytes.NewReader(src)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src[0] = 'X' // mutate the caller's slice after the write

	if _, body := get(t, s, n.Key, n.Run, n.Name, n.Author); body != "original" {
		t.Errorf("stored body mutated through the caller's slice: got %q", body)
	}

	_, rc, err := s.Get(t.Context(), n.Key, n.Run, n.Name, n.Author)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	returned, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	rc.Close()
	returned[0] = 'Y' // mutate the bytes Get handed back

	if _, body := get(t, s, n.Key, n.Run, n.Name, n.Author); body != "original" {
		t.Errorf("stored body mutated through the returned bytes: got %q", body)
	}
}

// runNoAliasing pins the invariant a path-laid-out backend can most easily
// break: a coordinate holding the path separator — or a percent-escape that
// looks like one — must address its own note and never another's. The in-memory
// backend gets this for free from Ref; a backend that lays notes out on an
// object path only gets it if its encoding is injective, and this is where an
// unescaped layout fails.
func runNoAliasing(t *testing.T, s note.Store) {
	// Every pair below frames identically under a naive "join with /" layout, so
	// each body must come back from exactly the note that stored it.
	notes := map[string]note.Note{
		"key holds the separator":    {Key: "a/b", Run: 1, Name: "c", Author: "x"},
		"name holds the separator":   {Key: "a", Run: 1, Name: "b/c", Author: "x"},
		"author holds the separator": {Key: "a", Run: 1, Name: "c", Author: "x/y"},
		"name holds a live escape":   {Key: "a", Run: 1, Name: "b%2Fc", Author: "x"},
		"key holds a percent":        {Key: "a%b", Run: 1, Name: "c", Author: "x"},
		"shared scope with a slash":  {Key: "a/b", Run: 1, Name: "c"},
	}
	for body, n := range notes {
		put(t, s, n, body)
	}

	for wantBody, n := range notes {
		if _, body := get(t, s, n.Key, n.Run, n.Name, n.Author); body != wantBody {
			t.Errorf("note %+v: got body %q, want %q — coordinates aliased", n, body, wantBody)
		}
	}

	// A filter narrows on the decoded coordinate, not on a path fragment: Key "a"
	// must not pick up the notes stored under Key "a/b".
	assertList(t, s, note.Filter{Key: "a"}, []note.Note{
		{Key: "a", Run: 1, Name: "b%2Fc", Author: "x"},
		{Key: "a", Run: 1, Name: "b/c", Author: "x"},
		{Key: "a", Run: 1, Name: "c", Author: "x/y"},
	})
	assertList(t, s, note.Filter{Key: "a/b"}, []note.Note{
		{Key: "a/b", Run: 1, Name: "c"},
		{Key: "a/b", Run: 1, Name: "c", Author: "x"},
	})
}

// runListFilter pins every Filter subset against one seeded run, including the
// subsets a prefix scan cannot express on its own (a run or name without a key,
// an author alone), which a backend must narrow with Filter.Matches.
func runListFilter(t *testing.T, s note.Store) {
	seed := []note.Note{
		{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"},
		{Key: "K", Run: 1, Name: "fix_plan", Author: "gemini"},
		{Key: "K", Run: 1, Name: "fix_plan"}, // shared / synthesizer scope
		{Key: "K", Run: 1, Name: "critique", Author: "claude"},
		{Key: "K", Run: 2, Name: "fix_plan", Author: "claude"},
		{Key: "other", Run: 1, Name: "fix_plan", Author: "claude"},
	}
	for _, n := range seed {
		put(t, s, n, "body of "+n.Name)
	}

	run1, run2 := 1, 2
	gemini, absent := "gemini", "nobody"
	for _, tc := range []struct {
		name   string
		filter note.Filter
		want   []note.Note
	}{{
		name:   "by key",
		filter: note.Filter{Key: "K"},
		want:   seed[:5],
	}, {
		name:   "by key and run",
		filter: note.Filter{Key: "K", Run: &run1},
		want:   seed[:4],
	}, {
		name:   "by key, run and name — the adversarial fan-in across authors",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan"},
		want:   seed[:3],
	}, {
		name:   "by author alone, no key",
		filter: note.Filter{Author: &gemini},
		want:   []note.Note{seed[1]},
	}, {
		name:   "by run alone, no key",
		filter: note.Filter{Run: &run2},
		want:   []note.Note{seed[4]},
	}, {
		name:   "by name alone, across keys",
		filter: note.Filter{Name: "fix_plan"},
		want:   []note.Note{seed[0], seed[1], seed[2], seed[4], seed[5]},
	}, {
		name:   "no filter matches every note",
		filter: note.Filter{},
		want:   seed,
	}, {
		name:   "no match",
		filter: note.Filter{Key: "K", Author: &absent},
		want:   nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			assertList(t, s, tc.filter, tc.want)
		})
	}
}

// runListPagination pins that a listing is bounded and complete: no page exceeds
// Filter.Limit, paging by Page.Cursor covers every match exactly once, and the
// walk terminates. It deliberately does not assert a page count — a backend
// whose storage query narrows only part of a filter returns short pages, which
// the contract allows.
func runListPagination(t *testing.T, s note.Store) {
	const (
		total = 12
		limit = 5
	)
	want := make([]note.Note, 0, total)
	for i := range total {
		n := note.Note{Key: "K", Run: 1, Name: fmt.Sprintf("step_%02d", i), Author: "claude"}
		put(t, s, n, "body")
		want = append(want, n)
	}
	// A second key the filter must exclude, so paging is not trivially "everything".
	put(t, s, note.Note{Key: "other", Run: 1, Name: "step_00", Author: "claude"}, "body")

	seen := make(map[note.Note]int, total)
	cursor := ""
	for calls := 0; ; calls++ {
		if calls > total+2 {
			t.Fatal("pagination did not terminate")
		}
		page, err := s.List(t.Context(), note.Filter{Key: "K", Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Notes) > limit {
			t.Fatalf("page over Limit: got %d notes, want <= %d", len(page.Notes), limit)
		}
		for _, n := range page.Notes {
			seen[n]++
		}
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}

	got := make([]note.Note, 0, len(seen))
	for n, count := range seen {
		if count != 1 {
			t.Errorf("note %+v returned %d times, want exactly once", n, count)
		}
		got = append(got, n)
	}
	if diff := cmp.Diff(SortNotes(want), SortNotes(got)); diff != "" {
		t.Errorf("paged coverage (-want, +got):\n%s", diff)
	}
}

// put stores body under n, failing the test on error.
func put(t *testing.T, s note.Store, n note.Note, body string) {
	t.Helper()
	if err := s.Put(t.Context(), n, strings.NewReader(body)); err != nil {
		t.Fatalf("Put(%+v): %v", n, err)
	}
}

// get returns a note's coordinates and body, failing the test on error. It
// closes the body reader, which every backend requires of a caller.
func get(t *testing.T, s note.Store, key string, run int, name, author string) (note.Note, string) {
	t.Helper()
	n, rc, err := s.Get(t.Context(), key, run, name, author)
	if err != nil {
		t.Fatalf("Get(%q, %d, %q, %q): %v", key, run, name, author, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body of (%q, %d, %q, %q): %v", key, run, name, author, err)
	}
	return n, string(body)
}

// assertList pages f to completion and compares the coordinates it yields with
// want as a set. It pages rather than taking the first result so a filter
// assertion cannot pass only because a backend happened to fit everything in one
// page.
func assertList(t *testing.T, s note.Store, f note.Filter, want []note.Note) {
	t.Helper()

	var got []note.Note
	for calls := 0; ; calls++ {
		if calls > len(want)+8 {
			t.Fatalf("List(%+v): pagination did not terminate", f)
		}
		page, err := s.List(t.Context(), f)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		got = append(got, page.Notes...)
		if page.Cursor == "" {
			break
		}
		f.Cursor = page.Cursor
	}

	if len(want) == 0 && len(got) == 0 {
		return
	}
	if diff := cmp.Diff(SortNotes(want), SortNotes(got)); diff != "" {
		t.Errorf("List(%+v) (-want, +got):\n%s", f, diff)
	}
}

// SortNotes returns notes in canonical coordinate order, so a comparison is
// independent of the order a backend happens to list in — the in-memory store
// orders by [note.Ref] and a blob-backed one by object name, and neither order
// is part of the contract. It is exported because every backend's own tests
// need it too, and a second copy would be one more thing to keep in step.
//
// The input is not modified.
func SortNotes(notes []note.Note) []note.Note {
	out := slices.Clone(notes)
	slices.SortFunc(out, func(a, b note.Note) int {
		switch {
		case a.Key != b.Key:
			return strings.Compare(a.Key, b.Key)
		case a.Run != b.Run:
			return a.Run - b.Run
		case a.Name != b.Name:
			return strings.Compare(a.Name, b.Name)
		default:
			return strings.Compare(a.Author, b.Author)
		}
	})
	return out
}
