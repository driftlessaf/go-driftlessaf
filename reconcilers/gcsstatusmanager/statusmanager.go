/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstatusmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/storage"
)

// Status captures reconciliation progress for a key.
type Status[T any] struct {
	// ObservedGeneration is set by the caller to track which version
	// of the resource was last processed (e.g. a commit SHA).
	ObservedGeneration string `json:"observedGeneration"`

	// Details contains reconciler-specific state data.
	Details T `json:"details"`
}

// Manager reads and writes reconciliation status as JSON objects in GCS.
type Manager[T any] struct {
	identity string
	bucket   *storage.BucketHandle
	readOnly bool
}

// New creates a new Manager that can read and write status.
// The identity is used as a prefix in the GCS object path: "{identity}/{key}".
func New[T any](identity string, bucket *storage.BucketHandle) *Manager[T] {
	return &Manager[T]{
		identity: identity,
		bucket:   bucket,
	}
}

// NewReadOnly creates a new Manager that can only read status.
func NewReadOnly[T any](identity string, bucket *storage.BucketHandle) *Manager[T] {
	return &Manager[T]{
		identity: identity,
		bucket:   bucket,
		readOnly: true,
	}
}

// Session represents reconciliation state for a single key.
type Session[T any] struct {
	manager *Manager[T]
	key     string
}

// NewSession creates a new reconciliation session for the given key.
// The key combined with the manager's identity forms the GCS object path: "{identity}/{key}".
// Returns an error if the key contains path traversal sequences.
func (m *Manager[T]) NewSession(key string) (*Session[T], error) {
	if err := validateKey(key); err != nil {
		return nil, fmt.Errorf("invalid key: %w", err)
	}
	return &Session[T]{
		manager: m,
		key:     key,
	}, nil
}

// validateKey checks that the key doesn't contain path traversal sequences.
func validateKey(key string) error {
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("key must not start with '/': %q", key)
	}
	if slices.Contains(strings.Split(key, "/"), "..") {
		return fmt.Errorf("key must not contain '..': %q", key)
	}
	return nil
}

// objectName returns the GCS object name for this session.
func (s *Session[T]) objectName() string {
	return s.manager.identity + "/" + s.key
}

// ObservedState reads the current status from GCS.
// Returns nil, nil if no status exists for this key.
func (s *Session[T]) ObservedState(ctx context.Context) (*Status[T], error) {
	r, err := s.manager.bucket.Object(s.objectName()).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading status object: %w", err)
	}
	defer r.Close()

	var status Status[T]
	if err := json.NewDecoder(r).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding status: %w", err)
	}

	return &status, nil
}

// SetActualState writes the status to GCS, overwriting any existing status.
func (s *Session[T]) SetActualState(ctx context.Context, status *Status[T]) error {
	if s.manager.readOnly {
		return errors.New("cannot set actual state: status manager is read-only")
	}
	if status == nil {
		return errors.New("status cannot be nil")
	}

	w := s.manager.bucket.Object(s.objectName()).NewWriter(ctx)
	w.ContentType = "application/json"

	if err := json.NewEncoder(w).Encode(status); err != nil {
		w.Close() // Best-effort close on encode error.
		return fmt.Errorf("encoding status: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("closing status writer: %w", err)
	}

	return nil
}
