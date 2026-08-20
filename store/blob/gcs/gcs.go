/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"chainguard.dev/driftlessaf/store/blob"
)

// Store is a GCS-backed blob store over a single bucket. The zero value is not
// usable; call New. It is safe for concurrent use — all state lives in GCS.
type Store struct {
	bucket *storage.BucketHandle
}

// New returns a Store backed by bucket.
func New(bucket *storage.BucketHandle) *Store {
	return &Store{bucket: bucket}
}

// Put writes data at name subject to cond, returning the object's new
// generation. A failed precondition (DoesNotExist against an existing object,
// or a stale GenerationMatch) is reported as blob.ErrPreconditionFailed.
func (s *Store) Put(ctx context.Context, name string, data []byte, cond blob.Cond) (blob.Gen, error) {
	obj := s.bucket.Object(name)
	if conds, ok := conditions(cond); ok {
		obj = obj.If(conds)
	}
	w := obj.NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	_, writeErr := w.Write(data)
	// Always Close, and prefer its error: storage.Writer uploads asynchronously,
	// so Close carries the authoritative server response (e.g. a 412 on a failed
	// precondition) even when Write already returned a pipe-level error.
	closeErr := w.Close()
	if err := putErr(name, writeErr, closeErr); err != nil {
		return 0, err
	}
	return blob.Gen(w.Attrs().Generation), nil
}

// putErr selects the authoritative error from a Write/Close pair and wraps it:
// Close (the server's response) wins when non-nil, Write is the fallback, and
// nil means the upload committed.
func putErr(name string, writeErr, closeErr error) error {
	switch {
	case closeErr != nil:
		return fmt.Errorf("blob put %q: %w", name, mapErr(closeErr))
	case writeErr != nil:
		return fmt.Errorf("blob put %q: %w", name, mapErr(writeErr))
	default:
		return nil
	}
}

// Get returns the object's bytes and generation; ok is false (nil data, zero
// Gen, nil error) when no object exists at name.
func (s *Store) Get(ctx context.Context, name string) ([]byte, blob.Gen, bool, error) {
	r, err := s.bucket.Object(name).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("blob get %q: %w", name, mapErr(err))
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, false, fmt.Errorf("blob get %q: %w", name, mapErr(err))
	}
	return data, blob.Gen(r.Attrs.Generation), true, nil
}

// Delete removes name. With ifGen == 0 the delete is unconditional; with
// ifGen > 0 it removes the object only if its generation matches. An
// unconditional delete of an absent object is reported as blob.ErrNotExist; a
// generation-conditional delete against an absent object or a differing
// generation is reported as blob.ErrPreconditionFailed (GCS returns 412 — the
// precondition can never hold).
func (s *Store) Delete(ctx context.Context, name string, ifGen blob.Gen) error {
	obj := s.bucket.Object(name)
	if ifGen != 0 {
		obj = obj.If(storage.Conditions{GenerationMatch: int64(ifGen)})
	}
	if err := mapErr(obj.Delete(ctx)); err != nil {
		return fmt.Errorf("blob delete %q: %w", name, err)
	}
	return nil
}

// defaultListLimit caps a List that does not set a positive limit, so a listing
// over a wide prefix never enumerates an unbounded bucket in one call.
const defaultListLimit = 100

// List returns a bounded Page of the objects whose name has the given prefix. It
// caps at limit (or defaultListLimit when limit <= 0) and sets Page.Cursor (the
// GCS page token) when more objects remain; pass that cursor back for the next
// page. Bodies are not fetched; callers read them lazily with Get.
func (s *Store) List(ctx context.Context, prefix string, limit int, cursor string) (blob.Page, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	pager := iterator.NewPager(it, limit, cursor)

	var attrs []*storage.ObjectAttrs
	next, err := pager.NextPage(&attrs)
	if err != nil {
		return blob.Page{}, fmt.Errorf("blob list %q: %w", prefix, mapErr(err))
	}
	page := blob.Page{Objects: make([]blob.Object, len(attrs)), Cursor: next}
	for i, a := range attrs {
		page.Objects[i] = blob.Object{Name: a.Name, Gen: blob.Gen(a.Generation)}
	}
	return page, nil
}

// conditions translates a blob.Cond into storage.Conditions. ok is false for the
// zero Cond, so the caller skips Object.If — GCS rejects an all-zero Conditions.
// DoesNotExist takes precedence over GenerationMatch, matching blob.Cond.
func conditions(cond blob.Cond) (storage.Conditions, bool) {
	switch {
	case cond.DoesNotExist:
		return storage.Conditions{DoesNotExist: true}, true
	case cond.GenerationMatch != 0:
		return storage.Conditions{GenerationMatch: int64(cond.GenerationMatch)}, true
	default:
		return storage.Conditions{}, false
	}
}

// mapErr translates GCS client errors into the blob sentinels: not-found →
// blob.ErrNotExist, precondition-failed (a stale generation or a failed
// DoesNotExist) → blob.ErrPreconditionFailed. Other errors pass through so
// callers can retry transient failures.
//
// mapErr, isNotFound, and isPreconditionFailed duplicate the GCS error mapping
// in agents/checkpoint/gcsstore (backend.go); store/blob is the natural home to
// consolidate the copies.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case isNotFound(err):
		return blob.ErrNotExist
	case isPreconditionFailed(err):
		return blob.ErrPreconditionFailed
	default:
		return err
	}
}

// isNotFound and isPreconditionFailed recognize both the HTTP client's
// googleapi.Error codes and the gRPC client's status codes, so a bucket handle
// from either storage.NewClient or storage.NewGRPCClient maps correctly.
func isNotFound(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
		return true
	}
	return status.Code(err) == codes.NotFound
}

func isPreconditionFailed(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed {
		return true
	}
	return status.Code(err) == codes.FailedPrecondition
}
