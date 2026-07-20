/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gcsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/storage"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// Sealer wraps and unwraps the on-disk representation of an envelope. It is the
// seam that keeps the KMS/AEAD SDK out of this package: production passes a
// KMS-envelope implementation, tests pass IdentitySealer. Seal is applied just
// before an envelope is written to GCS; Open is applied just after it is read.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

// IdentitySealer is a no-op Sealer that returns its input unchanged. It is for
// tests and local dev only; it provides no confidentiality.
type IdentitySealer struct{}

// Seal returns plaintext unchanged.
func (IdentitySealer) Seal(plaintext []byte) ([]byte, error) { return plaintext, nil }

// Open returns ciphertext unchanged.
func (IdentitySealer) Open(ciphertext []byte) ([]byte, error) { return ciphertext, nil }

// Store is a GCS-backed checkpoint.Store. The zero value is not usable; call
// New. It is safe for concurrent use (all state is in GCS and the immutable
// identity/sealer/backend fields).
type Store struct {
	identity string
	sealer   Sealer
	backend  objectBackend
}

var _ checkpoint.Store = (*Store)(nil)

// New returns a Store that parks envelopes in bucket under the "{identity}/"
// prefix, sealing each envelope with sealer before it is written.
func New(identity string, bucket *storage.BucketHandle, sealer Sealer) *Store {
	return &Store{
		identity: identity,
		sealer:   sealer,
		backend:  &gcsBackend{bucket: bucket},
	}
}

// objectName maps a key to its single GCS object name, rejecting keys that
// could escape the identity prefix.
func (s *Store) objectName(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("invalid key: %w", err)
	}
	return s.identity + "/" + key, nil
}

// validateKey checks that the key doesn't contain path traversal sequences.
// Copied from gcsstatusmanager to keep the two prefixing schemes identical.
func validateKey(key string) error {
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("key must not start with '/': %q", key)
	}
	if slices.Contains(strings.Split(key, "/"), "..") {
		return fmt.Errorf("key must not contain '..': %q", key)
	}
	return nil
}

// Save seals env and writes it under key unconditionally, overwriting any
// existing envelope and advancing the object's GCS generation so Tokens loaded
// before the re-save can no longer claim it (see the package doc). It never
// returns ErrTokenMismatch.
func (s *Store) Save(ctx context.Context, key string, env *checkpoint.Envelope) error {
	name, err := s.objectName(key)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling envelope: %w", err)
	}
	sealed, err := s.sealer.Seal(raw)
	if err != nil {
		return fmt.Errorf("sealing envelope: %w", err)
	}
	if err := s.backend.write(ctx, name, sealed); err != nil {
		return fmt.Errorf("writing checkpoint object %q: %w", name, err)
	}
	return nil
}

// Load reads, opens, and decodes the envelope under key, returning the object's
// GCS generation as the CAS Token. ok is false (nil envelope, zero Token, nil
// error) when no object exists.
func (s *Store) Load(ctx context.Context, key string) (*checkpoint.Envelope, checkpoint.Token, bool, error) {
	name, err := s.objectName(key)
	if err != nil {
		return nil, checkpoint.Token{}, false, err
	}
	sealed, gen, ok, err := s.backend.read(ctx, name)
	if err != nil {
		return nil, checkpoint.Token{}, false, fmt.Errorf("reading checkpoint object %q: %w", name, err)
	}
	if !ok {
		return nil, checkpoint.Token{}, false, nil
	}
	raw, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, checkpoint.Token{}, false, fmt.Errorf("opening envelope %q: %w", name, err)
	}
	var env checkpoint.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, checkpoint.Token{}, false, fmt.Errorf("decoding envelope %q: %w", name, err)
	}
	return &env, checkpoint.Token{Generation: gen}, true, nil
}

// Delete removes the envelope under key only if tok still matches the stored
// object's generation, returning checkpoint.ErrTokenMismatch otherwise —
// including when the object is already gone or tok is the zero Token, which
// can never match a live object.
func (s *Store) Delete(ctx context.Context, key string, tok checkpoint.Token) error {
	name, err := s.objectName(key)
	if err != nil {
		return err
	}
	// GCS generations are always positive, so a non-positive Token can never
	// match a stored object: fail closed without a backend call. Passing zero
	// through would reach the GCS client as an all-zero Conditions struct,
	// which it rejects with a plain error instead of a precondition failure.
	if tok.Generation <= 0 {
		return checkpoint.ErrTokenMismatch
	}
	return s.backend.deleteIfGen(ctx, name, tok.Generation)
}
