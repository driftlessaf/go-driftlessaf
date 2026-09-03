/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

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

// TestRef pins that Ref is deterministic, that every coordinate is load-bearing,
// and that its length-prefixed framing is injective — no coordinate value can be
// shifted across a field boundary to forge another note's Ref.
func TestRef(t *testing.T) {
	base := note.Ref("harden:widget", 1, "fix_plan", "claude")

	// Deterministic: same coordinates, same Ref.
	if again := note.Ref("harden:widget", 1, "fix_plan", "claude"); again != base {
		t.Errorf("Ref not deterministic: got %q and %q", base, again)
	}

	// Every coordinate changes the Ref.
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"key", note.Ref("harden:gadget", 1, "fix_plan", "claude")},
		{"run", note.Ref("harden:widget", 2, "fix_plan", "claude")},
		{"name", note.Ref("harden:widget", 1, "critique", "claude")},
		{"author", note.Ref("harden:widget", 1, "fix_plan", "gemini")},
	} {
		if tc.ref == base {
			t.Errorf("Ref ignores the %s coordinate", tc.name)
		}
	}

	// Adjacent string coordinates must not collide: (name, author) ("a","bc") vs
	// ("ab","c") — bare concatenation would make both "abc".
	if note.Ref("k", 1, "a", "bc") == note.Ref("k", 1, "ab", "c") {
		t.Error("Ref collides on adjacent coordinates")
	}
	// Embedded separator bytes must not let a coordinate cross a boundary: a NUL
	// separator would frame ("a","\x00b") and ("a\x00","b") identically.
	// Length-prefixing keeps them distinct regardless of content.
	if note.Ref("k", 1, "a", "\x00b") == note.Ref("k", 1, "a\x00", "b") {
		t.Error("Ref collides on an embedded NUL — framing is not injective")
	}
}

// TestNote_Validate pins the single storable-note gate: Key and Name required,
// Run >= 1, Author and body optional.
func TestNote_Validate(t *testing.T) {
	tests := []struct {
		name    string
		note    note.Note
		wantErr bool
	}{
		{"valid", note.Note{Key: "k", Run: 1, Name: "fix_plan", Author: "claude"}, false},
		{"valid without author (shared scope)", note.Note{Key: "k", Run: 1, Name: "fix_plan"}, false},
		{"zero value", note.Note{}, true},
		{"missing key", note.Note{Run: 1, Name: "n"}, true},
		{"missing name", note.Note{Key: "k", Run: 1}, true},
		{"run zero", note.Note{Key: "k", Run: 0, Name: "n"}, true},
		{"run negative", note.Note{Key: "k", Run: -1, Name: "n"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.note.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate(%+v): got err=%v, wantErr=%t", tt.note, err, tt.wantErr)
			}
		})
	}
}

func TestMem_PutGetRoundTrip(t *testing.T) {
	ctx := t.Context()
	store := note.NewMem()

	want := note.Note{Key: "harden:widget", Run: 3, Name: "fix_plan", Author: "claude"}
	const wantBody = "the plan"
	if err := store.Put(ctx, want, strings.NewReader(wantBody)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, rc, err := store.Get(ctx, want.Key, want.Run, want.Name, want.Author)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get coordinates (-want, +got):\n%s", diff)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("body: got %q, want %q", body, wantBody)
	}

	if _, _, err := store.Get(ctx, want.Key, want.Run, want.Name, "gemini"); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get(absent): got %v, want ErrNotExist", err)
	}
}

// TestMem_Put_RejectsInvalid pins that Put fails closed: an invalid note is
// rejected by the shared Note.Validate gate and nothing is stored.
func TestMem_Put_RejectsInvalid(t *testing.T) {
	ctx := t.Context()
	store := note.NewMem()

	if err := store.Put(ctx, note.Note{Key: "k", Run: 1}, nil); err == nil { // no Name
		t.Fatal("Put(invalid): got nil error, want a validation error")
	}
	if page, _ := store.List(ctx, note.Filter{}); len(page.Notes) != 0 {
		t.Errorf("invalid note was stored: %d notes present, want 0", len(page.Notes))
	}
}

func TestMem_BodyIsolation(t *testing.T) {
	ctx := t.Context()
	store := note.NewMem()

	src := []byte("original")
	if err := store.Put(ctx, note.Note{Key: "k", Run: 1, Name: "n"}, bytes.NewReader(src)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src[0] = 'X' // mutate caller's slice after the write

	body := readBody(t, store, "k", 1, "n", "")
	if want := "original"; string(body) != want {
		t.Errorf("stored body mutated through caller slice: got %q, want %q", body, want)
	}
	body[0] = 'Y' // mutate the returned bytes
	if again := readBody(t, store, "k", 1, "n", ""); string(again) != "original" {
		t.Errorf("stored body mutated through returned bytes: got %q", again)
	}
}

// readBody Gets a note's body and returns it, failing the test on any error.
func readBody(t *testing.T, s *note.Mem, key string, run int, name, author string) []byte {
	t.Helper()
	_, rc, err := s.Get(t.Context(), key, run, name, author)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// ident is a note's coordinates, used to compare List results as sets
// independent of the opaque Ref order.
type ident struct {
	Key    string
	Run    int
	Name   string
	Author string
}

func identsOf(notes []note.Note) []ident {
	out := make([]ident, 0, len(notes))
	for _, n := range notes {
		out = append(out, ident{n.Key, n.Run, n.Name, n.Author})
	}
	slices.SortFunc(out, func(a, b ident) int {
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

func TestMem_List_Filter(t *testing.T) {
	ctx := t.Context()
	store := note.NewMem()

	// Two authors plus a shared (Author: "") note, two runs, two note names, under
	// one key. The shared note shares (Key, Run, Name) with the authored fix_plan
	// notes so selecting the shared scope must be distinguishable from the author
	// wildcard.
	seed := []note.Note{
		{Key: "K", Run: 1, Name: "fix_plan", Author: "claude"},
		{Key: "K", Run: 1, Name: "fix_plan", Author: "gemini"},
		{Key: "K", Run: 1, Name: "fix_plan", Author: ""}, // shared / synthesizer scope
		{Key: "K", Run: 1, Name: "critique", Author: "claude"},
		{Key: "K", Run: 2, Name: "fix_plan", Author: "claude"},
		{Key: "other", Run: 1, Name: "fix_plan", Author: "claude"},
	}
	for _, n := range seed {
		if err := store.Put(ctx, n, nil); err != nil {
			t.Fatalf("Put %+v: %v", n, err)
		}
	}

	run1, run2 := 1, 2
	gemini, shared, nope := "gemini", "", "nonexistent"
	tests := []struct {
		name   string
		filter note.Filter
		want   []ident
	}{{
		name:   "by key",
		filter: note.Filter{Key: "K"},
		want: []ident{
			{"K", 1, "critique", "claude"}, {"K", 1, "fix_plan", ""},
			{"K", 1, "fix_plan", "claude"}, {"K", 1, "fix_plan", "gemini"},
			{"K", 2, "fix_plan", "claude"},
		},
	}, {
		name:   "by key+run+name, author wildcard (the adversarial fan-in)",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan"},
		want: []ident{
			{"K", 1, "fix_plan", ""}, {"K", 1, "fix_plan", "claude"}, {"K", 1, "fix_plan", "gemini"},
		},
	}, {
		name:   "shared scope selected exactly (&\"\", not the wildcard)",
		filter: note.Filter{Key: "K", Run: &run1, Name: "fix_plan", Author: &shared},
		want:   []ident{{"K", 1, "fix_plan", ""}},
	}, {
		name:   "by author",
		filter: note.Filter{Author: &gemini},
		want:   []ident{{"K", 1, "fix_plan", "gemini"}},
	}, {
		name:   "by run only",
		filter: note.Filter{Run: &run2},
		want:   []ident{{"K", 2, "fix_plan", "claude"}},
	}, {
		name:   "by name across keys",
		filter: note.Filter{Name: "fix_plan"},
		want: []ident{
			{"K", 1, "fix_plan", ""}, {"K", 1, "fix_plan", "claude"},
			{"K", 1, "fix_plan", "gemini"}, {"K", 2, "fix_plan", "claude"},
			{"other", 1, "fix_plan", "claude"},
		},
	}, {
		name:   "empty filter matches all",
		filter: note.Filter{},
		want: []ident{
			{"K", 1, "critique", "claude"}, {"K", 1, "fix_plan", ""},
			{"K", 1, "fix_plan", "claude"}, {"K", 1, "fix_plan", "gemini"},
			{"K", 2, "fix_plan", "claude"}, {"other", 1, "fix_plan", "claude"},
		},
	}, {
		name:   "no match",
		filter: note.Filter{Key: "K", Author: &nope},
		want:   []ident{},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.List(ctx, tt.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if page.Cursor != "" {
				t.Errorf("unexpected cursor %q (result fit in one page)", page.Cursor)
			}
			if diff := cmp.Diff(tt.want, identsOf(page.Notes)); diff != "" {
				t.Errorf("List (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestMem_List_Pagination(t *testing.T) {
	ctx := t.Context()
	store := note.NewMem()

	const total = 25
	for i := range total {
		if err := store.Put(ctx, note.Note{Key: "K", Run: 1, Name: fmt.Sprintf("n%02d", i)}, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := make(map[ident]int, total)
	pages := 0
	cursor := ""
	for {
		page, err := store.List(ctx, note.Filter{Key: "K", Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.Notes) > 10 {
			t.Fatalf("page over Limit: got %d, want <= 10", len(page.Notes))
		}
		for _, n := range page.Notes {
			seen[ident{n.Key, n.Run, n.Name, n.Author}]++
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
		t.Errorf("paged coverage: got %d distinct notes, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("note %+v returned %d times, want exactly once", id, count)
		}
	}
	if pages != 3 { // 25 items at Limit 10 → 10 + 10 + 5
		t.Errorf("page count: got %d, want 3", pages)
	}
}
