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
// Gets it back by the same ones. The body is streamed separately (see [Store]),
// not carried on the struct, so a note of any size moves without buffering it in
// full.
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

	// List returns a bounded Page of the coordinates matching f, in a stable
	// order; bodies are fetched separately with Get. When Page.Cursor is non-empty
	// there are more matches: pass it back in Filter.Cursor for the next page. A
	// cursor is only valid replayed against the same Filter (its other fields
	// unchanged); pairing it with a different filter is undefined.
	List(ctx context.Context, f Filter) (Page, error)
}
