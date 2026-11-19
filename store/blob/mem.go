/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package blob

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Mem is an in-memory blob backend for tests and single-process use. It is safe
// for concurrent use. The zero value is not usable; call NewMem.
type Mem struct {
	mu      sync.Mutex
	now     func() time.Time
	nextGen Gen
	objects map[string]memObject
}

type memObject struct {
	data    []byte
	gen     Gen
	created time.Time
}

// NewMem returns an empty in-memory blob backend. It records each generation's
// creation time from the wall clock.
func NewMem() *Mem {
	return NewMemWithClock(time.Now)
}

// NewMemWithClock returns an empty in-memory blob backend that records each
// generation's creation time by calling now when the write succeeds.
func NewMemWithClock(now func() time.Time) *Mem {
	return &Mem{
		now:     now,
		nextGen: 1,
		objects: make(map[string]memObject),
	}
}

// Put writes data at name subject to cond, returning the object's new
// generation. It fails with ErrPreconditionFailed when cond is not satisfied.
func (m *Mem) Put(_ context.Context, name string, data []byte, cond Cond) (Gen, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cur, exists := m.objects[name]
	// DoesNotExist takes precedence over GenerationMatch (matching Cond and the
	// GCS backend's conditions()), so they are separate arms, not one combined
	// condition — otherwise a create-only Put of an absent object would wrongly
	// trip the GenerationMatch arm.
	switch {
	case cond.DoesNotExist:
		if exists {
			return 0, ErrPreconditionFailed
		}
	case cond.GenerationMatch != 0:
		if !exists || cur.gen != cond.GenerationMatch {
			return 0, ErrPreconditionFailed
		}
	}

	gen := m.nextGen
	m.nextGen++
	// Clone so a caller mutating data after the write can't alter stored bytes.
	m.objects[name] = memObject{data: slices.Clone(data), gen: gen, created: m.now()}
	return gen, nil
}

// Get returns the object's bytes and generation; ok is false (nil data, zero
// Gen, nil error) when no object exists at name.
func (m *Mem) Get(_ context.Context, name string) ([]byte, Gen, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[name]
	if !ok {
		return nil, 0, false, nil
	}
	// Clone so callers can't mutate stored bytes through the returned slice.
	return slices.Clone(o.data), o.gen, true, nil
}

// GetVersion returns the object's current generation with its bytes and
// creation time; ok is false (zero Version, nil error) when no object exists at
// name.
func (m *Mem) GetVersion(_ context.Context, name string) (Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[name]
	if !ok {
		return Version{}, false, nil
	}
	return Version{Data: slices.Clone(o.data), Gen: o.gen, Created: o.created}, true, nil
}

// Delete removes name. With ifGen == 0 the delete is unconditional; with
// ifGen > 0 it removes the object only if its generation matches. An
// unconditional delete of an absent object returns ErrNotExist; a
// generation-conditional delete returns ErrPreconditionFailed when the object is
// absent or its generation differs (the generation can never match), matching
// the GCS backend.
func (m *Mem) Delete(_ context.Context, name string, ifGen Gen) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[name]
	switch {
	case !ok && ifGen == 0:
		return ErrNotExist
	case !ok:
		return ErrPreconditionFailed
	case ifGen != 0 && o.gen != ifGen:
		return ErrPreconditionFailed
	}
	delete(m.objects, name)
	return nil
}

// defaultListLimit caps a List that does not set a positive limit, so a listing
// over a wide prefix never returns an unbounded slice.
const defaultListLimit = 100

// List returns a bounded, name-ordered Page of the objects whose name has the
// given prefix. It caps at limit (or defaultListLimit when limit <= 0) and sets
// Page.Cursor when more objects remain; pass that cursor back for the next page.
// Bodies are not returned; fetch them with Get.
func (m *Mem) List(_ context.Context, prefix string, limit int, cursor string) (Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 {
		limit = defaultListLimit
	}

	names := make([]string, 0, len(m.objects))
	for name := range m.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	// Start strictly after the cursor's name.
	start := 0
	if cursor != "" {
		i, found := slices.BinarySearch(names, cursor)
		start = i
		if found {
			start++
		}
	}

	end := min(start+limit, len(names))
	page := Page{Objects: make([]Object, 0, end-start)}
	for _, name := range names[start:end] {
		page.Objects = append(page.Objects, Object{Name: name, Gen: m.objects[name].gen})
	}
	if end < len(names) {
		page.Cursor = names[end-1]
	}
	return page, nil
}
