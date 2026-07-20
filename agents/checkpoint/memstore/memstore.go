/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package memstore

import (
	"context"
	"sync"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// Store is an in-memory checkpoint.Store. The zero value is not usable; call
// New. It is safe for concurrent use.
type Store struct {
	mu  sync.Mutex
	gen int64
	m   map[string]entry
}

type entry struct {
	env *checkpoint.Envelope
	gen int64
}

// New returns an empty in-memory Store.
func New() *Store {
	return &Store{m: make(map[string]entry)}
}

var _ checkpoint.Store = (*Store)(nil)

// Save writes a deep copy of env under key and advances the CAS generation.
func (s *Store) Save(_ context.Context, key string, env *checkpoint.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.m[key] = entry{env: env.Clone(), gen: s.gen}
	return nil
}

// Load returns a deep copy of the stored envelope and its CAS token.
func (s *Store) Load(_ context.Context, key string) (*checkpoint.Envelope, checkpoint.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return nil, checkpoint.Token{}, false, nil
	}
	return e.env.Clone(), checkpoint.Token{Generation: e.gen}, true, nil
}

// Delete removes key only if tok matches the current generation.
func (s *Store) Delete(_ context.Context, key string, tok checkpoint.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || e.gen != tok.Generation {
		return checkpoint.ErrTokenMismatch
	}
	delete(s.m, key)
	return nil
}
