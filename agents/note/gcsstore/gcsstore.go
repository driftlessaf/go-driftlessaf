/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/chainguard-dev/clog"

	"chainguard.dev/driftlessaf/agents/note"
	"chainguard.dev/driftlessaf/store/blob"
)

// Blobs is the blob-backend method set this store drives. It is declared here —
// by the consumer — rather than exported by store/blob, following "accept
// interfaces, return structs": both *blob.Mem and the GCS backend satisfy it
// implicitly, so the store is exercised in unit tests against the in-memory
// backend and deployed against a bucket without changing.
type Blobs interface {
	Put(ctx context.Context, name string, data []byte, cond blob.Cond) (blob.Gen, error)
	Get(ctx context.Context, name string) ([]byte, blob.Gen, bool, error)
	Delete(ctx context.Context, name string, ifGen blob.Gen) error
	List(ctx context.Context, prefix string, limit int, cursor string) (blob.Page, error)
}

// DefaultMaxNoteSize is the body ceiling a Store uses unless WithMaxNoteSize
// overrides it. A ceiling exists so an unbounded or hostile body fails the write
// instead of exhausting the process: the blob primitive is byte-oriented, so a
// body is read into memory to be stored, and a read with no ceiling is the one
// way this store can be made to consume arbitrary memory.
const DefaultMaxNoteSize int64 = 64 << 20 // 64 MiB

// Store is a note.Store over a blob backend. The zero value is not usable; call
// New. It is safe for concurrent use — all state lives in the backend and the
// immutable configuration.
type Store struct {
	root        string
	blobs       Blobs
	maxNoteSize int64
}

// Option configures a Store at construction.
type Option func(*Store)

// WithMaxNoteSize sets the body ceiling, overriding DefaultMaxNoteSize. A
// non-positive size is ignored.
func WithMaxNoteSize(size int64) Option {
	return func(s *Store) {
		if size > 0 {
			s.maxNoteSize = size
		}
	}
}

var _ note.Store = (*Store)(nil)

// New returns a Store that keeps notes under root in blobs.
//
// root is the store's namespace and its tenancy boundary: notes are written and
// listed only beneath it, so a caller pins one store per tenant (an org-scoped
// bucket, project, or prefix) and hands a sandbox that store. Isolation is a
// property of the handle a caller is given, never of a coordinate the caller
// fills in — see the package doc. A root that does not end in "/" gets one, so
// one tenant's root cannot prefix another's ("org-1" would otherwise scan
// "org-10" too).
//
// A root that is absolute or contains a ".." segment is rejected, matching the
// prefixing rules agents/checkpoint/gcsstore and gcsstatusmanager apply to their
// own keys. root is deployer configuration rather than caller input, so this is
// checked once here instead of on every call: it is the tenancy boundary, and a
// boundary worth pinning is worth validating.
func New(root string, blobs Blobs, opts ...Option) (*Store, error) {
	if blobs == nil {
		return nil, errors.New("gcsstore: New requires a blob backend")
	}
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	s := &Store{root: root, blobs: blobs, maxNoteSize: DefaultMaxNoteSize}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// validateRoot rejects a root that is absolute or walks upward. Copied from
// agents/checkpoint/gcsstore to keep the two prefixing schemes identical.
func validateRoot(root string) error {
	if strings.HasPrefix(root, "/") {
		return fmt.Errorf("gcsstore: root must not start with %q: %q", "/", root)
	}
	if slices.Contains(strings.Split(root, "/"), "..") {
		return fmt.Errorf("gcsstore: root must not contain %q: %q", "..", root)
	}
	return nil
}

// Put reads body to completion and stores it under n's coordinates, overwriting
// any existing note with those coordinates. It returns the error from
// n.Validate when n is not a storable note, without touching the backend.
func (s *Store) Put(ctx context.Context, n note.Note, body io.Reader) error {
	if err := n.Validate(); err != nil {
		return err
	}
	data, err := readBody(body, s.maxNoteSize)
	if err != nil {
		return err
	}
	objName := objectName(s.root, n.Key, n.Run, n.Name, n.Author)
	if _, err := s.blobs.Put(ctx, objName, data, blob.Cond{}); err != nil {
		return fmt.Errorf("writing note object %q: %w", objName, err)
	}
	return nil
}

// readBody reads body into memory, rejecting a body over max rather than letting
// it grow unbounded. A nil body is empty. It reads one byte past max when
// representable so the limit is detected without trusting a length the caller
// reports.
func readBody(body io.Reader, max int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	limit := max
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return nil, fmt.Errorf("reading note body: %w", err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("note body exceeds the %d byte limit", max)
	}
	return data, nil
}

// Get returns the note's coordinates and a reader over its body, or
// note.ErrNotExist when no note is stored under the coordinates. The body is
// buffered, so the returned reader holds no backend connection; it is a
// ReadCloser to satisfy note.Store, and closing it is still required of callers
// so they behave the same against every backend.
func (s *Store) Get(ctx context.Context, key string, run int, name, author string) (note.Note, io.ReadCloser, error) {
	objName := objectName(s.root, key, run, name, author)
	data, _, ok, err := s.blobs.Get(ctx, objName)
	if err != nil {
		return note.Note{}, nil, fmt.Errorf("reading note object %q: %w", objName, err)
	}
	if !ok {
		return note.Note{}, nil, note.ErrNotExist
	}
	return note.Note{Key: key, Run: run, Name: name, Author: author}, io.NopCloser(bytes.NewReader(data)), nil
}

// Delete removes the note stored under the coordinates, or returns
// note.ErrNotExist when there is none.
//
// The delete is unconditional (generation 0), matching Put: the blob backend
// reports an unconditional delete of an absent object as blob.ErrNotExist, which
// maps to note.ErrNotExist so callers branch on one sentinel regardless of
// backend.
func (s *Store) Delete(ctx context.Context, key string, run int, name, author string) error {
	objName := objectName(s.root, key, run, name, author)
	switch err := s.blobs.Delete(ctx, objName, 0); {
	case errors.Is(err, blob.ErrNotExist):
		return note.ErrNotExist
	case err != nil:
		return fmt.Errorf("deleting note object %q: %w", objName, err)
	}
	return nil
}

// List returns a bounded Page of the coordinates matching f, recovered from
// object names so no body is read.
//
// A prefix scan narrows only f's leading coordinates (see listPrefix), so the
// remainder is applied with note.Filter.Matches — the same rule every backend
// selects by. That means a page can be short, or empty, while Page.Cursor is
// non-empty: the scan covers a bounded window of objects, not a bounded number
// of matches. Callers page until Page.Cursor is empty, as note.Store documents.
func (s *Store) List(ctx context.Context, f note.Filter) (note.Page, error) {
	// Filter.Limit and the blob limit are both "at most this many", and every
	// object in the window yields at most one note, so passing it through keeps
	// the page within Limit. Limit 0 leaves the default to the blob backend,
	// matching "0 = the store's default cap".
	objects, err := s.blobs.List(ctx, listPrefix(s.root, f), f.Limit, f.Cursor)
	if err != nil {
		return note.Page{}, fmt.Errorf("listing notes under %q: %w", listPrefix(s.root, f), err)
	}

	page := note.Page{Notes: make([]note.Note, 0, len(objects.Objects)), Cursor: objects.Cursor}
	for _, o := range objects.Objects {
		n, err := parseObjectName(s.root, o.Name)
		switch {
		case errors.Is(err, errNotANote):
			// Not one of this store's objects. A root can be shared with other
			// data, so skip rather than breaking the listing for everything
			// under it.
			clog.DebugContextf(ctx, "note: skipping non-note object %q: %v", o.Name, err)
			continue
		case err != nil:
			// Note-shaped but corrupt or non-canonical — the aliasing the
			// encoding exists to prevent. Fail closed rather than report a note
			// some other name also maps to.
			return note.Page{}, err
		}
		if f.Matches(n) {
			page.Notes = append(page.Notes, n)
		}
	}
	return page, nil
}
