/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ErrOutOfScope is returned by a [Scoped] handle for any operation addressing a
// note outside its scope. It is deliberately distinct from [ErrNotExist]:
// reaching past a grant must not be confused with a missing note and retried
// as though the requested note might become available within that grant.
var ErrOutOfScope = errors.New("note: outside the handle's scope")

// Scope is the span of notes a handle may touch: one workqueue key, and
// optionally one run of it.
//
// Key is required, because a run number alone is not a scope — run 1 exists
// under every key, so pinning only a run would leave a handle reaching every
// item in the store. Run is optional: it pins one attempt, or 0 admits every
// run of Key.
//
// The two settings are the grant ladder. A Scope with a Run is what a sandbox
// receives: the notes of one attempt of one item, which is exactly the span the
// linked workqueues of a pipeline hand off through (a step in one queue writes a
// note the next queue's step reads back under the same key and run). A Scope
// without a Run is the broader grant an orchestrator needs — retry comparing
// this attempt with the last, a rag layer embedding past runs, a synthesizer
// reconciling attempts. Broader still is the unscoped store, which is not a
// Scope at all.
//
// Unlike [Filter.Run], which uses nil for any run, Scope.Run uses the value 0.
// Keeping the grant in value fields lets a handle copy it without retaining
// caller-owned pointers that could change its scope.
type Scope struct {
	Key string // required — the workqueue key whose notes the handle may touch
	Run int    // 0 admits every run of Key; >= 1 pins one attempt
}

// Validate reports whether s is a usable scope.
func (s Scope) Validate() error {
	switch {
	case s.Key == "":
		return errors.New("note: Scope.Key is required")
	case s.Run < 0:
		return fmt.Errorf("note: Scope.Run must be >= 0, got %d", s.Run)
	default:
		return nil
	}
}

// permits reports whether the coordinates fall inside s.
func (s Scope) permits(key string, run int) bool {
	return key == s.Key && (s.Run == 0 || run == s.Run)
}

// contains reports whether every note other permits is also permitted by s —
// that is, whether other is no wider than s. A run-pinned scope contains only
// the same pinning; a run-wildcard scope contains any run of the same key.
func (s Scope) contains(other Scope) bool {
	return other.Key == s.Key && (s.Run == 0 || other.Run == s.Run)
}

// Scoped is a [Store] confined to one [Scope]: the capability handle a sandbox
// or an orchestrator is given. Every Put, Get, Delete, and List is checked
// against the scope before it reaches the wrapped store, and the scope is fixed
// at construction — there is no setter and no way to widen a handle after it is
// issued, so a grant frozen when it was minted stays frozen.
//
// This is where note.Store's isolation lives. A note's coordinates are
// content-addressed so that any agent can compute a peer's [Ref] without a
// lookup, which is what makes note-passing work — and what makes a coordinate
// useless as an authorization check, since an agent could compute another
// scope's just as easily. So the boundary is the handle a caller holds, never a
// field in the note: the coordinate coordinates, the capability isolates.
//
// Scoped is one axis of the isolation, not all of it. It confines a handle
// within a store; the store's own namespace (see the gcsstore root) is where the
// per-tenant floor and the queue live. A Scoped over a tenant's store isolates
// one item's run; it says nothing about another tenant, whose notes are in a
// different store entirely.
//
// It is safe for concurrent use when the wrapped store is. The zero value is
// not usable; call [NewScoped].
type Scoped struct {
	scope Scope
	inner Store
}

var _ Store = (*Scoped)(nil)

// NewScoped returns a handle onto inner confined to scope.
//
// When inner exposes Scope() Scope, scope must be no wider than that grant,
// and NewScoped fails otherwise. Decorators should forward Scope so this check
// also applies through them. A decorator that hides Scope still delegates to
// the inner handle's per-operation checks, but prevents this construction-time
// check.
//
// Nesting is how a grant is narrowed — an orchestrator holding a key-wide
// handle issuing a run-pinned one to a step —
// and the check keeps a re-scope from reading as a widening: without it,
// wrapping a run-3 handle in a run-wildcard Scope would mint something that
// looks like a broader grant while every out-of-run call fails deeper down.
func NewScoped(scope Scope, inner Store) (*Scoped, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, errors.New("note: NewScoped requires a store to wrap")
	}
	if outer, ok := inner.(interface{ Scope() Scope }); ok {
		if grant := outer.Scope(); !grant.contains(scope) {
			return nil, fmt.Errorf("note: scope %+v is wider than the handle it wraps (%+v): a handle can only be narrowed", scope, grant)
		}
	}
	return &Scoped{scope: scope, inner: inner}, nil
}

// Scope returns the scope frozen into the handle. It returns a copy, so a
// caller cannot widen a live handle through the value it reads back.
func (s *Scoped) Scope() Scope {
	return s.scope
}

// Put stores the note when it falls inside the handle's scope, and returns
// ErrOutOfScope otherwise.
//
// The scope is checked before the note is validated, so a note that is both
// out of scope and unstorable reports ErrOutOfScope: the boundary is answered
// first, and the wrapped store applies Note.Validate as it would for any
// caller.
func (s *Scoped) Put(ctx context.Context, n Note, body io.Reader) error {
	if !s.scope.permits(n.Key, n.Run) {
		return fmt.Errorf("%w: cannot put (key %q, run %d) under scope %+v", ErrOutOfScope, n.Key, n.Run, s.scope)
	}
	return s.inner.Put(ctx, n, body)
}

// Get returns the note when it falls inside the handle's scope, and
// ErrOutOfScope otherwise.
//
// Reaching past the scope is reported as a scope failure rather than as
// ErrNotExist. A caller that addresses the wrong run receives ErrOutOfScope
// regardless of whether that note exists, so it cannot mistake a denied read
// for "the upstream note is not ready yet" and retry it indefinitely.
func (s *Scoped) Get(ctx context.Context, key string, run int, name, author string) (Note, io.ReadCloser, error) {
	if !s.scope.permits(key, run) {
		return Note{}, nil, fmt.Errorf("%w: cannot get (key %q, run %d) under scope %+v", ErrOutOfScope, key, run, s.scope)
	}
	return s.inner.Get(ctx, key, run, name, author)
}

// Delete removes the note when it falls inside the handle's scope, and returns
// ErrOutOfScope otherwise.
//
// A handle that can write within its scope can also remove within it: both are
// mutations of the same span, and a run that may publish a note may retract it.
// What a run-scoped handle cannot do is reach into a sibling run to delete its
// notes, which is the case this gate exists for — a retention sweep across runs
// is the broader grant (a Scope with no Run, or the un-pinned store), issued to
// the orchestrator that owns retention.
func (s *Scoped) Delete(ctx context.Context, key string, run int, name, author string) error {
	if !s.scope.permits(key, run) {
		return fmt.Errorf("%w: cannot delete (key %q, run %d) under scope %+v", ErrOutOfScope, key, run, s.scope)
	}
	return s.inner.Delete(ctx, key, run, name, author)
}

// List returns the matching notes inside the handle's scope.
//
// A wildcard is resolved to the scope rather than refused: a filter with no Key
// means "everything I can see", which for a scoped handle is its own scope, so
// Key and Run are filled in from it. A filter naming a different key or run is
// an explicit reach past the grant and returns ErrOutOfScope. The effect is that
// a handle can never list beyond its scope, whether the caller asked narrowly or
// not.
//
// Because the narrowing is a pure function of the scope and the caller's filter,
// replaying a Page.Cursor with the same filter reproduces the same underlying
// listing, so paging works exactly as it does on the wrapped store.
// Notes outside the scope are removed from the returned page even if the
// backend ignores the narrowed filter. The backend's cursor is preserved, so
// callers must continue paging when a filtered page is empty but has a cursor.
func (s *Scoped) List(ctx context.Context, f Filter) (Page, error) {
	switch {
	case f.Key == "":
		f.Key = s.scope.Key
	case f.Key != s.scope.Key:
		return Page{}, fmt.Errorf("%w: cannot list key %q under scope %+v", ErrOutOfScope, f.Key, s.scope)
	}

	if s.scope.Run != 0 {
		if f.Run != nil {
			if run := *f.Run; run != s.scope.Run {
				return Page{}, fmt.Errorf("%w: cannot list run %d under scope %+v", ErrOutOfScope, run, s.scope)
			}
		}
		// Forward an owned value so changes to the caller's run cannot widen
		// the listing after the scope check.
		f.Run = new(s.scope.Run)
	}
	page, err := s.inner.List(ctx, f)
	if err != nil {
		return Page{}, err
	}
	page.Notes = slices.DeleteFunc(slices.Clone(page.Notes), func(n Note) bool {
		return !s.scope.permits(n.Key, n.Run)
	})
	return page, nil
}
