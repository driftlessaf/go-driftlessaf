/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/note"
)

// hostileCoordinates are coordinate sets whose values contain the characters the
// layout has to survive: the path separator, a live percent-escape, a raw
// percent, path-traversal dots, a NUL, and the empty author.
var hostileCoordinates = []note.Note{
	{Key: "harden:widget", Run: 1, Name: "fix_plan", Author: "claude"},
	{Key: "a/b", Run: 1, Name: "c", Author: "x"},
	{Key: "a", Run: 1, Name: "b/c", Author: "x"},
	{Key: "a", Run: 1, Name: "b", Author: "c/x"}, // aliases the line above if segments carry no role prefix
	{Key: "a", Run: 1, Name: "c", Author: "x/y"},
	{Key: "a", Run: 1, Name: "b%2Fc", Author: "x"},
	{Key: "a%b", Run: 1, Name: "c%2F", Author: "x%"},
	{Key: "..", Run: 1, Name: "..", Author: ".."},
	{Key: "/leading", Run: 1, Name: "trailing/", Author: "/"},
	{Key: "a\x00b", Run: 1, Name: "c\x00d", Author: "e\x00f"},
	{Key: "run_1", Run: 1, Name: "author_x", Author: "key_k"},
	{Key: "k", Run: 1, Name: "n"}, // the shared scope
	{Key: "k", Run: 1_000_000, Name: "n", Author: "a"},
	{Key: "üñí", Run: 1, Name: "søren", Author: "日本"},
}

// TestObjectName_RoundTrips pins that every coordinate survives encoding and
// decoding unchanged, so a listing reports the coordinates a note was stored
// under rather than a mangled approximation of them.
func TestObjectName_RoundTrips(t *testing.T) {
	for _, root := range []string{"", "notes/", "orgs/example/"} {
		for _, want := range hostileCoordinates {
			objName := objectName(root, want.Key, want.Run, want.Name, want.Author)
			got, err := parseObjectName(root, objName)
			if err != nil {
				t.Errorf("parseObjectName(%q, %q): %v", root, objName, err)
				continue
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("round trip of %+v under root %q (-want, +got):\n%s", want, root, diff)
			}
		}
	}
}

// TestObjectName_SegmentsAreWellFormed pins the two structural properties the
// layout depends on: an escaped coordinate never introduces a path separator,
// and no segment is empty — including the shared scope's author segment, which
// would otherwise leave a trailing slash.
func TestObjectName_SegmentsAreWellFormed(t *testing.T) {
	for _, n := range hostileCoordinates {
		objName := objectName("notes/", n.Key, n.Run, n.Name, n.Author)
		rest := strings.TrimPrefix(objName, "notes/")
		segments := strings.Split(rest, "/")
		if len(segments) != segmentCount {
			t.Errorf("%+v encoded to %q: got %d segments, want %d — a coordinate leaked a separator", n, objName, len(segments), segmentCount)
			continue
		}
		for i, s := range segments {
			if s == "" {
				t.Errorf("%+v encoded to %q: segment %d is empty", n, objName, i)
			}
		}
	}
}

// TestObjectName_IsInjective pins the invariant the layout exists for: distinct
// coordinates never share an object name. Path equality has to be coordinate
// equality, since the object name is what the backend keys on — an unescaped
// layout fails here, aliasing ("a/b","c") onto ("a","b/c").
func TestObjectName_IsInjective(t *testing.T) {
	byName := make(map[string]note.Note, len(hostileCoordinates))
	for _, n := range hostileCoordinates {
		objName := objectName("notes/", n.Key, n.Run, n.Name, n.Author)
		if prior, clash := byName[objName]; clash {
			t.Errorf("object name %q is shared by %+v and %+v", objName, prior, n)
		}
		byName[objName] = n
	}

	// The name must also agree with Ref on which notes are the same note: two
	// coordinate sets share a name if and only if they share a Ref.
	for _, a := range hostileCoordinates {
		for _, b := range hostileCoordinates {
			sameName := objectName("notes/", a.Key, a.Run, a.Name, a.Author) == objectName("notes/", b.Key, b.Run, b.Name, b.Author)
			sameRef := note.Ref(a.Key, a.Run, a.Name, a.Author) == note.Ref(b.Key, b.Run, b.Name, b.Author)
			if sameName != sameRef {
				t.Errorf("name and Ref disagree on %+v vs %+v: sameName=%t sameRef=%t", a, b, sameName, sameRef)
			}
		}
	}
}

// TestParseObjectName_Classifies pins the split that decides whether a listing
// survives a shared root. A name without the note layout's shape is errNotANote,
// which List skips so the root can hold other data; a name WITH the shape but
// bad content is a plain error, which List treats as fatal. Getting this
// backwards either bricks every listing under a shared prefix or silently hides
// the aliasing the canonical check exists to catch.
func TestParseObjectName_Classifies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		objName  string
		notANote bool // true: skippable shape failure; false: fatal
	}{
		// Shape failures — skippable, because the root may be shared.
		{"outside the root", "other/key_k/run_1/name_n/author_a", true},
		{"a plain sibling file", "notes/SKILL.md", true},
		{"no segments", "notes/", true},
		{"too few segments", "notes/key_k/run_1/name_n", true},
		{"too many segments", "notes/key_k/run_1/name_n/author_a/more", true},
		{"key prefix missing", "notes/k/run_1/name_n/author_a", true},
		{"run prefix missing", "notes/key_k/1/name_n/author_a", true},
		{"name prefix missing", "notes/key_k/run_1/n/author_a", true},
		{"author prefix missing", "notes/key_k/run_1/name_n/a", true},
		{"invalid key and run prefix missing", "notes/key_%zz/1/name_n/author_a", true},
		{"invalid key and name prefix missing", "notes/key_%zz/run_1/n/author_a", true},
		{"invalid key and author prefix missing", "notes/key_%zz/run_1/name_n/a", true},
		{"invalid run and name prefix missing", "notes/key_k/run_assets/tree/file.txt", true},
		{"invalid run and author prefix missing", "notes/key_k/run_assets/name_n/file.txt", true},
		{"invalid name and author prefix missing", "notes/key_k/run_1/name_%zz/a", true},

		// Note-shaped but unusable — fatal, never skipped.
		{"run not a number", "notes/key_k/run_x/name_n/author_a", false},
		{"run zero fails Validate", "notes/key_k/run_0/name_n/author_a", false},
		{"run negative fails Validate", "notes/key_k/run_-1/name_n/author_a", false},
		{"empty key fails Validate", "notes/key_/run_1/name_n/author_a", false},
		{"empty name fails Validate", "notes/key_k/run_1/name_/author_a", false},
		{"run has a leading zero", "notes/key_k/run_01/name_n/author_a", false},
		{"run has a plus sign", "notes/key_k/run_+1/name_n/author_a", false},
		{"escape uses lowercase hex", "notes/key_a%2fb/run_1/name_n/author_a", false},
		{"value escaped when it need not be", "notes/key_%6B/run_1/name_n/author_a", false},
		{"truncated escape", "notes/key_a%2/run_1/name_n/author_a", false},
		{"invalid escape", "notes/key_a%zz/run_1/name_n/author_a", false},
		{"invalid name escape", "notes/key_k/run_1/name_%zz/author_a", false},
		{"invalid author escape", "notes/key_k/run_1/name_n/author_%zz", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseObjectName("notes/", tc.objName)
			if err == nil {
				t.Fatalf("parseObjectName(%q): got %+v, want an error", tc.objName, got)
			}
			if isNotANote := errors.Is(err, errNotANote); isNotANote != tc.notANote {
				t.Errorf("parseObjectName(%q): errNotANote=%t, want %t (err: %v)", tc.objName, isNotANote, tc.notANote, err)
			}
		})
	}
}

// TestListPrefix pins that a filter narrows the scan in path order and stops at
// its first wildcard — the residue is the caller's job, so a prefix that reached
// past a wildcard would silently drop matching notes.
func TestListPrefix(t *testing.T) {
	run1 := 1
	claude, shared := "claude", ""
	for _, tc := range []struct {
		name   string
		filter note.Filter
		want   string
	}{{
		name:   "no filter scans the whole root",
		filter: note.Filter{},
		want:   "notes/",
	}, {
		name:   "key",
		filter: note.Filter{Key: "K"},
		want:   "notes/key_K/",
	}, {
		name:   "key and run",
		filter: note.Filter{Key: "K", Run: &run1},
		want:   "notes/key_K/run_1/",
	}, {
		name:   "key, run and name — the fan-in",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan"},
		want:   "notes/key_K/run_1/name_fix_plan/",
	}, {
		name:   "all four coordinates",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan", Author: &claude},
		want:   "notes/key_K/run_1/name_fix_plan/author_claude",
	}, {
		name:   "the shared scope",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan", Author: &shared},
		want:   "notes/key_K/run_1/name_fix_plan/author_",
	}, {
		name:   "a run without a key cannot narrow",
		filter: note.Filter{Run: &run1},
		want:   "notes/",
	}, {
		name:   "a name without a run cannot narrow past the key",
		filter: note.Filter{Key: "K", Name: "fix_plan"},
		want:   "notes/key_K/",
	}, {
		name:   "an author without a name cannot narrow past the run",
		filter: note.Filter{Key: "K", Run: &run1, Author: &claude},
		want:   "notes/key_K/run_1/",
	}, {
		name:   "a key holding the separator is escaped into one segment",
		filter: note.Filter{Key: "a/b"},
		want:   "notes/key_a%2Fb/",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listPrefix("notes/", tc.filter); got != tc.want {
				t.Errorf("listPrefix: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListPrefix_IsAPrefixOfEveryMatch is the property the narrowing rests on:
// whatever a filter selects must live under the prefix the scan uses, or the
// scan would never see it.
func TestListPrefix_IsAPrefixOfEveryMatch(t *testing.T) {
	run1 := 1
	shared := ""
	filters := []note.Filter{
		{},
		{Key: "a"},
		{Key: "a", Run: &run1},
		{Key: "a", Run: &run1, Name: "c"},
		{Key: "a", Run: &run1, Name: "c", Author: &shared},
		{Run: &run1},
		{Name: "c"},
		{Key: "a/b"},
	}
	for _, f := range filters {
		prefix := listPrefix("notes/", f)
		for _, n := range hostileCoordinates {
			if !f.Matches(n) {
				continue
			}
			objName := objectName("notes/", n.Key, n.Run, n.Name, n.Author)
			if !strings.HasPrefix(objName, prefix) {
				t.Errorf("filter %+v matches %+v but its scan prefix %q does not cover %q", f, n, prefix, objName)
			}
		}
	}
}
