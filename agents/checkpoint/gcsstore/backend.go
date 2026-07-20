/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"context"
	"errors"
	"io"
	"net/http"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// objectBackend abstracts the subset of GCS object operations Store needs, so
// tests can substitute a hand-rolled in-memory fake without fake-gcs-server.
type objectBackend interface {
	// write unconditionally writes data at name, creating the object or
	// replacing an existing one. Every write advances the object's generation,
	// invalidating Tokens minted from earlier generations.
	write(ctx context.Context, name string, data []byte) error

	// read returns the object bytes and its generation. ok is false (nil data,
	// zero gen, nil error) when the object does not exist.
	read(ctx context.Context, name string) (data []byte, gen int64, ok bool, err error)

	// deleteIfGen deletes name only if its current generation equals gen,
	// returning checkpoint.ErrTokenMismatch on a mismatch or an absent object.
	deleteIfGen(ctx context.Context, name string, gen int64) error
}

// gcsBackend is the real objectBackend over a GCS bucket.
type gcsBackend struct {
	bucket *storage.BucketHandle
}

var _ objectBackend = (*gcsBackend)(nil)

func (b *gcsBackend) write(ctx context.Context, name string, data []byte) error {
	w := b.bucket.Object(name).NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(data); err != nil {
		_ = w.Close() // Best-effort close on write error.
		return err
	}
	return w.Close()
}

func (b *gcsBackend) read(ctx context.Context, name string) ([]byte, int64, bool, error) {
	r, err := b.bucket.Object(name).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, false, err
	}
	return data, r.Attrs.Generation, true, nil
}

func (b *gcsBackend) deleteIfGen(ctx context.Context, name string, gen int64) error {
	return mapDeleteErr(b.bucket.Object(name).If(storage.Conditions{GenerationMatch: gen}).Delete(ctx))
}

// mapDeleteErr translates the errors a generation-conditional GCS delete can
// return into the claim-once contract: precondition-failed (stale generation)
// and not-found (object already claimed or superseded) both mean the token no
// longer matches, so they surface as checkpoint.ErrTokenMismatch unwrapped.
// Anything else (transient/server errors) passes through raw so callers retry.
func mapDeleteErr(err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) || isPreconditionFailed(err) {
		return checkpoint.ErrTokenMismatch
	}
	return err
}

func isNotFound(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

func isPreconditionFailed(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed
}
