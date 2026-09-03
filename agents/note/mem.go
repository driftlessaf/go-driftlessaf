/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package note

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
)

// defaultListLimit caps a List that does not set Filter.Limit, so List never
// returns an unbounded slice.
const defaultListLimit = 1000

// Mem is an in-memory Store for tests and single-process use. It is safe for
// concurrent use. The zero value is not usable; call NewMem.
type Mem struct {
	mu    sync.Mutex
	notes map[string]memNote // keyed by Ref(...)
}

// memNote is a stored note: its coordinates plus the buffered body.
type memNote struct {
	note Note
	body []byte
}

// NewMem returns an empty in-memory note store.
func NewMem() *Mem {
	return &Mem{notes: make(map[string]memNote)}
}

var _ Store = (*Mem)(nil)

// Put validates n, reads body to completion, and stores it under n's Ref,
// overwriting any existing note with the same coordinates. The body is buffered,
// so a caller mutating its source after the write can't alter stored bytes.
func (m *Mem) Put(_ context.Context, n Note, body io.Reader) error {
	if err := n.Validate(); err != nil {
		return err
	}

	var buf []byte
	if body != nil {
		var err error
		if buf, err = io.ReadAll(body); err != nil {
			return fmt.Errorf("note: reading body: %w", err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.notes[Ref(n.Key, n.Run, n.Name, n.Author)] = memNote{note: n, body: buf}
	return nil
}

// Get returns the note's coordinates and a reader over a copy of its body, or
// ErrNotExist when absent. The reader needs no cleanup, but is a ReadCloser to
// satisfy Store so callers Close it uniformly across backends.
func (m *Mem) Get(_ context.Context, key string, run int, name, author string) (Note, io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.notes[Ref(key, run, name, author)]
	if !ok {
		return Note{}, nil, ErrNotExist
	}
	return e.note, io.NopCloser(bytes.NewReader(slices.Clone(e.body))), nil
}

// List returns a bounded, Ref-ordered Page of the coordinates matching f. It caps
// at Filter.Limit (or defaultListLimit) and sets Page.Cursor when more matches
// remain; pass that cursor back in Filter.Cursor for the next page.
func (m *Mem) List(_ context.Context, f Filter) (Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	limit := cmp.Or(max(f.Limit, 0), defaultListLimit)

	refs := make([]string, 0, len(m.notes))
	for ref, e := range m.notes {
		if matches(f, e.note) {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)

	// Start strictly after the cursor's Ref.
	start := 0
	if f.Cursor != "" {
		i, found := slices.BinarySearch(refs, f.Cursor)
		start = i
		if found {
			start++
		}
	}

	end := min(start+limit, len(refs))
	page := Page{Notes: make([]Note, 0, end-start)}
	for _, ref := range refs[start:end] {
		page.Notes = append(page.Notes, m.notes[ref].note)
	}
	if end < len(refs) {
		page.Cursor = refs[end-1]
	}
	return page, nil
}

// matches reports whether n satisfies every non-wildcard field of f.
func matches(f Filter, n Note) bool {
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
