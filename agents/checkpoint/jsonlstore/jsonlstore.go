/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package jsonlstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// record is one append-only line in the JSONL log.
type record struct {
	Key        string               `json:"key"`
	Generation int64                `json:"generation"`
	Deleted    bool                 `json:"deleted,omitempty"`
	Envelope   *checkpoint.Envelope `json:"envelope,omitempty"`
}

// Store is an append-only JSONL-backed checkpoint.Store. Create with New; safe
// for concurrent use within a single process.
type Store struct {
	mu    sync.Mutex
	path  string
	gen   int64
	index map[string]record // latest live record per key
}

var _ checkpoint.Store = (*Store)(nil)

// New opens (creating if absent) the JSONL log at path and replays it to
// rebuild live state.
func New(path string) (*Store, error) {
	s := &Store{path: path, index: make(map[string]record)}
	if err := s.replay(); err != nil {
		return nil, err
	}
	return s, nil
}

// replay scans the existing log to rebuild the index and the generation
// counter. A missing file is treated as an empty log. Lines are read with an
// unbounded per-line reader rather than a fixed-max-token scanner: an
// Envelope's ProviderState carries a whole serialized provider request
// (multi-turn history, thinking blocks, tool inputs), so a single record can
// legitimately outgrow any fixed buffer, and a capped scanner would turn one
// oversized checkpoint into a store that can never be opened again.
func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("jsonlstore: open %q: %w", s.path, err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var rec record
			if err := json.Unmarshal(trimmed, &rec); err != nil {
				return fmt.Errorf("jsonlstore: corrupt record in %q: %w", s.path, err)
			}
			if rec.Generation > s.gen {
				s.gen = rec.Generation
			}
			if rec.Deleted {
				delete(s.index, rec.Key)
			} else {
				s.index[rec.Key] = rec
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("jsonlstore: read %q: %w", s.path, readErr)
		}
	}
}

// append writes one record to the log, flushing to disk before returning.
func (s *Store) append(rec record) error {
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("jsonlstore: open for append %q: %w", s.path, err)
	}
	defer f.Close()

	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("jsonlstore: marshal record: %w", err)
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("jsonlstore: append %q: %w", s.path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("jsonlstore: sync %q: %w", s.path, err)
	}
	return nil
}

// Save appends a new record for key and advances the CAS generation.
func (s *Store) Save(_ context.Context, key string, env *checkpoint.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	rec := record{Key: key, Generation: s.gen, Envelope: env.Clone()}
	if err := s.append(rec); err != nil {
		s.gen-- // roll back the counter so a failed write leaves no gap
		return err
	}
	s.index[key] = rec
	return nil
}

// Load returns a deep copy of the latest live envelope for key and its token.
func (s *Store) Load(_ context.Context, key string) (*checkpoint.Envelope, checkpoint.Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.index[key]
	if !ok {
		return nil, checkpoint.Token{}, false, nil
	}
	return rec.Envelope.Clone(), checkpoint.Token{Generation: rec.Generation}, true, nil
}

// Delete appends a tombstone for key only if tok matches the live generation.
func (s *Store) Delete(_ context.Context, key string, tok checkpoint.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.index[key]
	if !ok || rec.Generation != tok.Generation {
		return checkpoint.ErrTokenMismatch
	}
	if err := s.append(record{Key: key, Generation: rec.Generation, Deleted: true}); err != nil {
		return err
	}
	delete(s.index, key)
	return nil
}
