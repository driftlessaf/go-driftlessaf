/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/agents/note/notetest"
)

// seeded returns a store holding notes across two keys and two runs, so a
// scoped handle over it has plenty in reach to leak.
func seeded(t *testing.T) *note.Mem {
	t.Helper()
	store := note.NewMem()
	for _, n := range []note.Note{
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
		{Key: "harden:widget", Run: 1, Name: "review/critique", Author: "claude"},
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"}, // shared scope
		{Key: "harden:widget", Run: 2, Name: "harden/fix_plan", Author: "claude"},
		{Key: "harden:gadget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
	} {
		if err := store.Put(t.Context(), n, strings.NewReader("body of "+n.Name)); err != nil {
			t.Fatalf("seed %+v: %v", n, err)
		}
	}
	return store
}

func TestScope_Validate(t *testing.T) {
	tests := []struct {
		name    string
		scope   note.Scope
		wantErr bool
	}{
		{"key and run", note.Scope{Key: "k", Run: 1}, false},
		{"key only admits every run", note.Scope{Key: "k"}, false},
		{"zero value", note.Scope{}, true},
		{"run without a key is not a scope", note.Scope{Run: 1}, true},
		{"negative run", note.Scope{Key: "k", Run: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.scope.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate(%+v): got err=%v, wantErr=%t", tt.scope, err, tt.wantErr)
			}
		})
	}
}

func TestNewScoped_RejectsUnusable(t *testing.T) {
	if _, err := note.NewScoped(note.Scope{Run: 1}, note.NewMem()); err == nil {
		t.Error("NewScoped(no key): got nil error, want a validation error")
	}
	if _, err := note.NewScoped(note.Scope{Key: "k"}, nil); err == nil {
		t.Error("NewScoped(nil store): got nil error, want an error")
	}
}

// TestScoped_InScopePassesThrough pins that the guard is transparent inside its
// scope: a handle is a Store, not a reduced one.
func TestScoped_InScopePassesThrough(t *testing.T) {
	handle, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 3}, note.NewMem())
	if err != nil {
		t.Fatalf("NewScoped: %v", err)
	}

	want := note.Note{Key: "harden:widget", Run: 3, Name: "harden/fix_plan", Author: "claude"}
	const wantBody = "rebuild the lockfile"
	if err := handle.Put(t.Context(), want, strings.NewReader(wantBody)); err != nil {
		t.Fatalf("Put(in scope): %v", err)
	}

	got, rc, err := handle.Get(t.Context(), want.Key, want.Run, want.Name, want.Author)
	if err != nil {
		t.Fatalf("Get(in scope): %v", err)
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

	// An absent note inside the scope is still ErrNotExist, not ErrOutOfScope.
	if _, _, err := handle.Get(t.Context(), want.Key, want.Run, "harden/absent", ""); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get(absent, in scope): got %v, want ErrNotExist", err)
	}
	if err := handle.Delete(t.Context(), want.Key, want.Run, "harden/absent", ""); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Delete(absent, in scope): got %v, want ErrNotExist", err)
	}

	// A run that may publish a note may retract it.
	if err := handle.Delete(t.Context(), want.Key, want.Run, want.Name, want.Author); err != nil {
		t.Errorf("Delete(in scope): %v", err)
	}
	if _, _, err := handle.Get(t.Context(), want.Key, want.Run, want.Name, want.Author); !errors.Is(err, note.ErrNotExist) {
		t.Errorf("Get after in-scope Delete: got %v, want ErrNotExist", err)
	}
}

// TestScoped_RejectsOutOfScope pins that every method refuses every way of
// addressing past the grant, on both axes.
func TestScoped_RejectsOutOfScope(t *testing.T) {
	scope := note.Scope{Key: "harden:widget", Run: 1}
	backing := seeded(t)
	handle, err := note.NewScoped(scope, backing)
	if err != nil {
		t.Fatalf("NewScoped: %v", err)
	}

	otherRun, otherKey := 2, "harden:gadget"
	for _, tc := range []struct {
		name string
		call func() error
	}{{
		name: "put another key",
		call: func() error {
			return handle.Put(t.Context(), note.Note{Key: otherKey, Run: 1, Name: "n"}, nil)
		},
	}, {
		name: "put another run",
		call: func() error {
			return handle.Put(t.Context(), note.Note{Key: scope.Key, Run: otherRun, Name: "n"}, nil)
		},
	}, {
		name: "get another key",
		call: func() error {
			_, _, err := handle.Get(t.Context(), otherKey, 1, "harden/fix_plan", "claude")
			return err
		},
	}, {
		name: "get another run",
		call: func() error {
			_, _, err := handle.Get(t.Context(), scope.Key, otherRun, "harden/fix_plan", "claude")
			return err
		},
	}, {
		name: "delete another key",
		call: func() error {
			return handle.Delete(t.Context(), otherKey, 1, "harden/fix_plan", "claude")
		},
	}, {
		name: "delete another run",
		call: func() error {
			return handle.Delete(t.Context(), scope.Key, otherRun, "harden/fix_plan", "claude")
		},
	}, {
		name: "list another key",
		call: func() error {
			_, err := handle.List(t.Context(), note.Filter{Key: otherKey})
			return err
		},
	}, {
		name: "list another run",
		call: func() error {
			_, err := handle.List(t.Context(), note.Filter{Run: &otherRun})
			return err
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, note.ErrOutOfScope) {
				t.Errorf("got %v, want ErrOutOfScope", err)
			}
		})
	}

	// A rejected Put must not have reached the store, and a rejected Delete must
	// have left its target intact.
	for _, rejected := range []note.Note{
		{Key: otherKey, Run: scope.Run, Name: "n"},
		{Key: scope.Key, Run: otherRun, Name: "n"},
	} {
		_, rc, err := backing.Get(t.Context(), rejected.Key, rejected.Run, rejected.Name, rejected.Author)
		if rc != nil {
			rc.Close()
		}
		if !errors.Is(err, note.ErrNotExist) {
			t.Errorf("Get after rejected Put(%+v): got %v, want ErrNotExist", rejected, err)
		}
	}
	for _, survivor := range []note.Note{
		{Key: otherKey, Run: 1, Name: "harden/fix_plan", Author: "claude"},
		{Key: scope.Key, Run: otherRun, Name: "harden/fix_plan", Author: "claude"},
	} {
		if _, _, err := backing.Get(t.Context(), survivor.Key, survivor.Run, survivor.Name, survivor.Author); err != nil {
			t.Errorf("a rejected Delete removed %+v: %v", survivor, err)
		}
	}
}

// beforeListStore runs a hook after Scoped checks the filter, then delegates
// the listing to the real store so filtering and paging keep their semantics.
type beforeListStore struct {
	note.Store
	beforeList func()
}

func (s *beforeListStore) List(ctx context.Context, f note.Filter) (note.Page, error) {
	s.beforeList()
	return s.Store.List(ctx, f)
}

func TestScoped_ListOwnsRunFilter(t *testing.T) {
	run := 1
	want := note.Note{Key: "harden:widget", Run: run, Name: "harden/fix_plan", Author: "claude"}
	handle, err := note.NewScoped(note.Scope{Key: want.Key, Run: run}, &beforeListStore{
		Store: seeded(t),
		// Mutate the caller's run after validation but before the backing
		// store reads the filter. No concurrent access is needed to expose
		// an alias that would let the listing reach another run.
		beforeList: func() { run = 2 },
	})
	if err != nil {
		t.Fatalf("NewScoped: %v", err)
	}
	page, err := handle.List(t.Context(), note.Filter{Run: &run, Name: want.Name, Author: new(want.Author)})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if diff := cmp.Diff([]note.Note{want}, page.Notes); diff != "" {
		t.Errorf("List after caller changes run (-want, +got):\n%s", diff)
	}
}

// listResultStore deliberately returns an unchecked page to test the scope
// boundary when a backend does not honor the supplied filter.
type listResultStore struct {
	note.Store
	page note.Page
	err  error
}

func (s *listResultStore) List(context.Context, note.Filter) (note.Page, error) {
	return s.page, s.err
}

func TestScoped_ListFiltersBackendResults(t *testing.T) {
	notes := []note.Note{
		{Key: "harden:gadget", Run: 1, Name: "harden/fix_plan"},
		{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"},
		{Key: "harden:widget", Run: 2, Name: "harden/fix_plan"},
	}
	backendErr := errors.New("listing failed")
	for _, tc := range []struct {
		name  string
		scope note.Scope
		err   error
		want  note.Page
	}{
		{
			name:  "run-pinned",
			scope: note.Scope{Key: "harden:widget", Run: 1},
			want:  note.Page{Notes: []note.Note{notes[1]}, Cursor: "next"},
		},
		{
			name:  "key-wide",
			scope: note.Scope{Key: "harden:widget"},
			want:  note.Page{Notes: notes[1:], Cursor: "next"},
		},
		{
			name:  "empty page keeps cursor",
			scope: note.Scope{Key: "harden:absent"},
			want:  note.Page{Notes: []note.Note{}, Cursor: "next"},
		},
		{
			name:  "backend error exposes no page",
			scope: note.Scope{Key: "harden:widget", Run: 1},
			err:   backendErr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := &listResultStore{
				page: note.Page{Notes: slices.Clone(notes), Cursor: "next"},
				err:  tc.err,
			}
			handle, err := note.NewScoped(tc.scope, backing)
			if err != nil {
				t.Fatalf("NewScoped: %v", err)
			}
			got, err := handle.List(t.Context(), note.Filter{})
			if !errors.Is(err, tc.err) {
				t.Fatalf("List error: got %v, want %v", err, tc.err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("List (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(note.Page{Notes: notes, Cursor: "next"}, backing.page); diff != "" {
				t.Errorf("backend page changed (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestScoped_ListNarrowsWildcards pins that a wildcard resolves to the scope
// rather than to the whole store — the case that decides whether a handle leaks.
func TestScoped_ListNarrowsWildcards(t *testing.T) {
	run1, run2 := 1, 2
	shared := ""
	for _, tc := range []struct {
		name   string
		scope  note.Scope
		filter note.Filter
		want   []note.Note
	}{{
		name:   "run-pinned handle, empty filter",
		scope:  note.Scope{Key: "harden:widget", Run: 1},
		filter: note.Filter{},
		want: []note.Note{
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
			{Key: "harden:widget", Run: 1, Name: "review/critique", Author: "claude"},
		},
	}, {
		name:   "run-pinned handle, name narrows further",
		scope:  note.Scope{Key: "harden:widget", Run: 1},
		filter: note.Filter{Name: "harden/fix_plan"},
		want: []note.Note{
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
		},
	}, {
		name:   "run-pinned handle, shared scope selected exactly",
		scope:  note.Scope{Key: "harden:widget", Run: 1},
		filter: note.Filter{Author: &shared},
		want:   []note.Note{{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"}},
	}, {
		name:   "key-wide handle sees every run of its key, and no other key",
		scope:  note.Scope{Key: "harden:widget"},
		filter: note.Filter{},
		want: []note.Note{
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
			{Key: "harden:widget", Run: 1, Name: "review/critique", Author: "claude"},
			{Key: "harden:widget", Run: 2, Name: "harden/fix_plan", Author: "claude"},
		},
	}, {
		name:   "key-wide handle may name a run",
		scope:  note.Scope{Key: "harden:widget"},
		filter: note.Filter{Run: &run1},
		want: []note.Note{
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "claude"},
			{Key: "harden:widget", Run: 1, Name: "harden/fix_plan", Author: "gemini"},
			{Key: "harden:widget", Run: 1, Name: "review/critique", Author: "claude"},
		},
	}, {
		name:   "naming its own key is not a reach",
		scope:  note.Scope{Key: "harden:widget", Run: 2},
		filter: note.Filter{Key: "harden:widget", Run: &run2},
		want:   []note.Note{{Key: "harden:widget", Run: 2, Name: "harden/fix_plan", Author: "claude"}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := note.NewScoped(tc.scope, seeded(t))
			if err != nil {
				t.Fatalf("NewScoped: %v", err)
			}
			page, err := handle.List(t.Context(), tc.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if diff := cmp.Diff(tc.want, notetest.SortNotes(page.Notes)); diff != "" {
				t.Errorf("List (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestScoped_LeaksNothing is the property the handle exists for: whatever the
// caller asks, nothing outside the scope comes back. It drives every filter
// shape a caller can build against a store seeded with out-of-scope notes.
func TestScoped_LeaksNothing(t *testing.T) {
	scope := note.Scope{Key: "harden:widget", Run: 1}
	handle, err := note.NewScoped(scope, seeded(t))
	if err != nil {
		t.Fatalf("NewScoped: %v", err)
	}

	run1, run2 := 1, 2
	claude, shared := "claude", ""
	filters := []note.Filter{
		{},
		{Key: "harden:widget"},
		{Run: &run1},
		{Run: &run2},
		{Name: "harden/fix_plan"},
		{Name: "review/critique"},
		{Author: &claude},
		{Author: &shared},
		{Key: "harden:widget", Run: &run1, Name: "harden/fix_plan", Author: &claude},
		{Limit: 1},
		{Limit: 100},
	}
	for _, f := range filters {
		page, err := handle.List(t.Context(), f)
		if errors.Is(err, note.ErrOutOfScope) {
			continue // an explicit reach past the grant, correctly refused
		}
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		for _, n := range page.Notes {
			if n.Key != scope.Key || n.Run != scope.Run {
				t.Errorf("List(%+v) leaked %+v, outside scope %+v", f, n, scope)
			}
		}
	}
}

type scopedDecorator struct {
	*note.Scoped
}

// TestScoped_Nesting pins that a grant can be narrowed but not widened, so a
// re-scope cannot read as a broader capability than the handle it wraps.
func TestScoped_Nesting(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(*note.Scoped) note.Store
	}{
		{"direct", func(s *note.Scoped) note.Store { return s }},
		{"decorated", func(s *note.Scoped) note.Store { return &scopedDecorator{Scoped: s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyWide, err := note.NewScoped(note.Scope{Key: "harden:widget"}, seeded(t))
			if err != nil {
				t.Fatalf("NewScoped(key-wide): %v", err)
			}

			// Narrowing a key-wide handle to one run is the orchestrator-to-step case.
			runPinned, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 1}, tc.wrap(keyWide))
			if err != nil {
				t.Fatalf("NewScoped(narrow to a run): %v", err)
			}
			run2 := 2
			if _, err := runPinned.List(t.Context(), note.Filter{Run: &run2}); !errors.Is(err, note.ErrOutOfScope) {
				t.Errorf("narrowed handle reaching run 2: got %v, want ErrOutOfScope", err)
			}

			// Widening is refused at construction, not left to fail deeper down.
			if _, err := note.NewScoped(note.Scope{Key: "harden:widget"}, tc.wrap(runPinned)); err == nil {
				t.Error("NewScoped(widen a run-pinned handle): got nil error, want a rejection")
			}
			if _, err := note.NewScoped(note.Scope{Key: "harden:gadget"}, tc.wrap(keyWide)); err == nil {
				t.Error("NewScoped(another key): got nil error, want a rejection")
			}
			// Re-pinning the same scope is not a widening.
			if _, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 1}, tc.wrap(runPinned)); err != nil {
				t.Errorf("NewScoped(same scope): %v", err)
			}
		})
	}
}

// TestScoped_ScopeIsFrozen pins that the scope a handle was minted with cannot
// be edited through the value Scope() hands back.
func TestScoped_ScopeIsFrozen(t *testing.T) {
	handle, err := note.NewScoped(note.Scope{Key: "harden:widget", Run: 1}, seeded(t))
	if err != nil {
		t.Fatalf("NewScoped: %v", err)
	}

	got := handle.Scope()
	got.Key = "harden:gadget"
	got.Run = 2

	if after := handle.Scope(); after.Key != "harden:widget" || after.Run != 1 {
		t.Errorf("scope mutated through the returned value: got %+v", after)
	}
	if _, _, err := handle.Get(t.Context(), "harden:gadget", 2, "harden/fix_plan", "claude"); !errors.Is(err, note.ErrOutOfScope) {
		t.Errorf("handle honored a mutated scope: got %v, want ErrOutOfScope", err)
	}
}
