/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Note is the identity of one note passed between agents: the four coordinates
// that address it. Because agents don't share memory, a note is passed by being
// persisted — a step Puts its body under these coordinates and a downstream step
// Gets it back by the same ones.
//
// The body is passed separately (see [Store]) rather than carried on the struct,
// so a note's size is not a property of its identity and a listing can report
// coordinates without moving bodies. Whether a backend streams a body
// end-to-end is a backend property, not a promise of this type: both the
// in-memory store and the blob-backed one hold a body in memory for the length
// of a call.
type Note struct {
	Key    string // workqueue key — the item the run is about (e.g. "harden:<skill>")
	Run    int    // run number — the pipeline attempt for this key, 1-based
	Name   string // step/note name: "fix_plan", "critique", …
	Author string // who produced it: a model, a synthesizer, or a human ("" for shared notes)
}

// Validate reports whether n has the minimum coordinates a store will accept. It
// is the single gate every backend's Put calls, so "what is a storable note" is
// defined once rather than drifting per backend. Author is optional ("" is the
// shared/synthesizer scope); the body may be empty.
func (n Note) Validate() error {
	switch {
	case n.Key == "":
		return errors.New("note: Note.Key is required")
	case n.Name == "":
		return errors.New("note: Note.Name is required")
	case n.Run < 1:
		return fmt.Errorf("note: Note.Run must be >= 1, got %d", n.Run)
	default:
		return nil
	}
}

// Ref is the deterministic primary key for a note: the hex SHA-256 of its four
// coordinates in canonical (Key, Run, Name, Author) order. Any agent can compute
// it from the coordinates alone, so a note is addressable without a lookup or an
// opaque handle. There is deliberately no Ref field on Note — the key is always
// derived, never stored, so it can't disagree with the coordinates.
//
// Each string coordinate is length-prefixed (an 8-byte big-endian length, then
// the bytes) rather than joined by a separator. Length-prefixing is injective for
// ALL inputs: field boundaries come from the lengths, not the content, so no
// coordinate value — even one containing the byte a separator would use — can be
// shifted across a boundary to forge another note's Ref. A bare separator (e.g.
// NUL) only disambiguates coordinates that never contain that byte; a coordinate
// free to hold it can collide, e.g. ("a","\x00b") vs ("a\x00","b").
func Ref(key string, run int, name, author string) string {
	h := sha256.New()
	writeField := func(s string) {
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(len(s)))
		_, _ = h.Write(l[:])
		_, _ = h.Write([]byte(s))
	}
	writeField(key)
	var r [8]byte
	binary.BigEndian.PutUint64(r[:], uint64(run))
	_, _ = h.Write(r[:])
	writeField(name)
	writeField(author)
	return hex.EncodeToString(h.Sum(nil))
}

// Filter selects notes by any subset of coordinates; empty/nil fields are
// wildcards. Limit and Cursor bound and page a List.
type Filter struct {
	Key    string  // "" matches any key
	Run    *int    // nil matches any run
	Name   string  // "" matches any name
	Author *string // nil matches any author; &"" selects the shared/synthesizer scope exactly
	Limit  int     // 0 = the store's default cap; List never returns an unbounded slice
	Cursor string  // opaque continuation from a prior Page for the SAME filter; "" = the first page
}

// Matches reports whether n satisfies every non-wildcard field of f. It is the
// single definition of what a Filter selects, so a backend that can narrow only
// part of a filter in its own storage query — a blob prefix covers a leading
// subset of the coordinates, never an arbitrary one — applies the remainder with
// the same rule the in-memory backend uses instead of a re-derived copy of it.
//
// Limit and Cursor bound and page a listing rather than selecting, so Matches
// ignores them.
func (f Filter) Matches(n Note) bool {
	switch {
	case f.Key != "" && n.Key != f.Key:
		return false
	case f.Run != nil && n.Run != *f.Run:
		return false
	case f.Name != "" && n.Name != f.Name:
		return false
	case f.Author != nil && n.Author != *f.Author:
		return false
	default:
		return true
	}
}

// Page is one bounded slice of a List plus the cursor for the next.
type Page struct {
	Notes  []Note
	Cursor string // "" when the listing is exhausted
}

// ErrNotExist is returned by Get when no note exists for the given coordinates.
var ErrNotExist = errors.New("note: not found")

// Store is the note-passing medium: a persistent read/write store of note bodies
// keyed by (Key, Run, Name, Author). It is the passing medium and the durable
// copy at once. A backend returns a concrete type implementing Store (see
// [NewMem]); a consumer accepts Store, or a narrower interface of just the
// methods it uses.
type Store interface {
	// Put stores body under Ref(n.Key, n.Run, n.Name, n.Author), overwriting any
	// existing note with those coordinates. It reads body to completion (a nil
	// body is empty) and returns the error from n.Validate when n is not a
	// storable note.
	Put(ctx context.Context, n Note, body io.Reader) error

	// Get returns the note's coordinates and an open reader over its body, or
	// ErrNotExist when no note is stored under the coordinates. The caller must
	// Close the returned reader.
	Get(ctx context.Context, key string, run int, name, author string) (Note, io.ReadCloser, error)

	// Delete removes the note stored under the coordinates, or returns
	// ErrNotExist when there is none — the same signal Get gives for the same
	// coordinates, so a caller that wants deletion to be idempotent treats
	// ErrNotExist as success rather than the store guessing which it meant.
	//
	// It addresses exactly one note. Deleting an author's note leaves its
	// siblings under the same (Key, Run, Name) alone, including the shared
	// Author "" scope. There is deliberately no delete-by-Filter: retention is
	// the application's policy, and it expresses a sweep as List-then-Delete,
	// where each removal is a separate durable decision rather than one call
	// that can fail half way through with no record of how far it got.
	//
	// Deletion is unconditional, like Put: a note has one writer per coordinate
	// set, so there is no generation to check and no lost update to detect.
	Delete(ctx context.Context, key string, run int, name, author string) error

	// List returns a bounded Page of the coordinates matching f, in a stable
	// order; bodies are fetched separately with Get. When Page.Cursor is non-empty
	// there are more matches: pass it back in Filter.Cursor for the next page. A
	// cursor is only valid replayed against the same Filter (its other fields
	// unchanged); pairing it with a different filter is undefined.
	//
	// A page may hold fewer notes than Filter.Limit — including none at all —
	// while Page.Cursor is still non-empty. A backend whose storage query narrows
	// only part of a filter scans a bounded window and drops the non-matching
	// remainder, so a short page means "this window held few matches", never
	// "the listing is done". Only an empty Page.Cursor ends a listing; a caller
	// that stops early on a short page silently misses notes.
	List(ctx context.Context, f Filter) (Page, error)
}
