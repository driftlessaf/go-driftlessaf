/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package rag

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// memoryStore is a test Store that records upserts in memory.
type memoryStore struct {
	mu      sync.Mutex
	records map[string]memoryRecord
	err     error // if set, Upsert returns this error
	closed  bool
}

type memoryRecord struct {
	vector   []float32
	metadata map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[string]memoryRecord)}
}

func (s *memoryStore) Upsert(_ context.Context, id string, vector []float32, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.records[id] = memoryRecord{vector: vector, metadata: metadata}
	return nil
}

func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memoryStore) get(id string) (memoryRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	return r, ok
}

func TestMultiStoreUpsertWritesToAllStores(t *testing.T) {
	s1 := newMemoryStore()
	s2 := newMemoryStore()
	ms := NewMultiStore(s1, s2)

	ctx := t.Context()
	vector := []float32{1.0, 2.0, 3.0}
	metadata := map[string]string{"key": "value"}

	if err := ms.Upsert(ctx, "test-id", vector, metadata); err != nil {
		t.Fatalf("Upsert: unexpected error: %v", err)
	}

	for i, s := range []*memoryStore{s1, s2} {
		r, ok := s.get("test-id")
		if !ok {
			t.Errorf("store %d: record not found", i)
			continue
		}
		if r.metadata["key"] != "value" {
			t.Errorf("store %d: metadata[key]: got = %q, want = %q", i, r.metadata["key"], "value")
		}
	}
}

func TestMultiStoreUpsertAttemptsAllOnError(t *testing.T) {
	s1 := newMemoryStore()
	s1.err = errors.New("store 1 failed")
	s2 := newMemoryStore()
	ms := NewMultiStore(s1, s2)

	ctx := t.Context()
	err := ms.Upsert(ctx, "test-id", []float32{1.0}, map[string]string{})

	if err == nil {
		t.Fatal("Upsert: expected error, got nil")
	}
	if !errors.Is(err, s1.err) {
		t.Errorf("error should wrap store 1 error: got = %v", err)
	}

	// Store 2 should still have been written to.
	if _, ok := s2.get("test-id"); !ok {
		t.Error("store 2: record not found — MultiStore should attempt all stores")
	}
}

func TestMultiStoreUpsertCollectsAllErrors(t *testing.T) {
	err1 := errors.New("store 1 failed")
	err2 := errors.New("store 2 failed")
	s1 := newMemoryStore()
	s1.err = err1
	s2 := newMemoryStore()
	s2.err = err2
	ms := NewMultiStore(s1, s2)

	ctx := t.Context()
	err := ms.Upsert(ctx, "test-id", []float32{1.0}, map[string]string{})

	if !errors.Is(err, err1) {
		t.Errorf("error should wrap store 1 error: got = %v", err)
	}
	if !errors.Is(err, err2) {
		t.Errorf("error should wrap store 2 error: got = %v", err)
	}
}

func TestMultiStoreCloseClosesAll(t *testing.T) {
	s1 := newMemoryStore()
	s2 := newMemoryStore()
	ms := NewMultiStore(s1, s2)

	if err := ms.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	if !s1.closed {
		t.Error("store 1 not closed")
	}
	if !s2.closed {
		t.Error("store 2 not closed")
	}
}

func TestMatchingEngineStoreValidation(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name     string
		location string
		index    string
		wantErr  string
	}{
		{"empty location", "", "projects/p/locations/l/indexes/i", "location is required"},
		{"empty index", "us-central1", "", "indexName is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMatchingEngineStore(ctx, tt.location, tt.index)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := err.Error(); got != tt.wantErr {
				t.Errorf("error: got = %q, want = %q", got, tt.wantErr)
			}
		})
	}
}

func TestStoreUpsertEmptyIDReturnsError(t *testing.T) {
	s := newMemoryStore()
	ms := NewMultiStore(s)

	// The in-memory store doesn't validate, but the real MatchingEngineStore
	// and GCSStore both validate empty IDs. Test the GCSStore validation
	// indirectly through the interface contract.
	ctx := t.Context()

	// GCSStore validates id.
	gcs := &GCSStore{bucket: "test-bucket"}
	err := gcs.Upsert(ctx, "", []float32{1.0}, nil)
	if err == nil {
		t.Error("GCSStore.Upsert with empty id: expected error, got nil")
	}

	_ = ms
}

func TestNewMultiStoreZeroStoresPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewMultiStore with zero stores: expected panic, got none")
		}
	}()
	NewMultiStore()
}
