/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Compile-time interface assertions.
var (
	_ Store = (*GCSStore)(nil)
	_ Store = (*MultiStore)(nil)
)

// gcsRecord is the JSON structure persisted in GCS for each datapoint.
// It stores the source text, metadata, and embedding vector so the index
// can be rebuilt from scratch (e.g., when upgrading embedding models).
type gcsRecord struct {
	ID         string            `json:"id"`
	SourceText string            `json:"source_text"`
	Metadata   map[string]string `json:"metadata"`
	Vector     []float32         `json:"vector"`
	StoredAt   string            `json:"stored_at"`
}

// GCSStore implements Store by persisting embedding records to Google Cloud Storage.
// Each datapoint is written as a JSON file at {prefix}/{id}.json.
//
// GCSStore is designed for durability, not search. Pair it with MatchingEngineStore
// via MultiStore for both durable persistence and real-time search.
type GCSStore struct {
	client *storage.Client
	bucket string
	prefix string // optional path prefix within the bucket
}

// NewGCSStore creates a store that persists records to GCS.
//
// Records are written to gs://{bucket}/{prefix}/{id}.json.
// The prefix is optional — pass "" to write to the bucket root.
func NewGCSStore(ctx context.Context, bucket, prefix string, opts ...option.ClientOption) (*GCSStore, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}

	client, err := storage.NewClient(ctx, append(opts, option.WithScopes(storage.ScopeReadWrite))...)
	if err != nil {
		return nil, fmt.Errorf("creating storage client: %w", err)
	}

	return &GCSStore{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}, nil
}

// Upsert writes a record to GCS. The source text used to generate the embedding
// should be passed in metadata under MetadataKeySourceText so it can be
// re-embedded when upgrading models. If not present, the vector is still stored
// but re-embedding will require the original source data from elsewhere.
func (s *GCSStore) Upsert(ctx context.Context, id string, vector []float32, metadata map[string]string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}

	record := gcsRecord{
		ID:         id,
		SourceText: metadata[MetadataKeySourceText],
		Metadata:   metadata,
		Vector:     vector,
		StoredAt:   time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshaling record: %w", err)
	}

	objectName := id + ".json"
	if s.prefix != "" {
		objectName = s.prefix + "/" + objectName
	}

	w := s.client.Bucket(s.bucket).Object(objectName).NewWriter(ctx)
	w.ContentType = "application/json"
	if _, err := w.Write(data); err != nil {
		closeErr := w.Close()
		return fmt.Errorf("writing to GCS: %w", errors.Join(err, closeErr))
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing GCS writer: %w", err)
	}

	return nil
}

// Close releases the storage client.
func (s *GCSStore) Close() error {
	return s.client.Close()
}

// MultiStore writes to multiple Store backends simultaneously.
// Use this to write to both GCS (durability) and Matching Engine (search).
type MultiStore struct {
	stores []Store
}

// NewMultiStore creates a store that fans out writes to all provided stores.
// At least one store must be provided; panics if called with zero stores.
func NewMultiStore(stores ...Store) *MultiStore {
	if len(stores) == 0 {
		panic("NewMultiStore requires at least one store")
	}
	return &MultiStore{stores: stores}
}

// Upsert writes to all stores. All stores are attempted regardless of
// individual failures; errors are collected and returned via errors.Join.
func (m *MultiStore) Upsert(ctx context.Context, id string, vector []float32, metadata map[string]string) error {
	var errs []error
	for _, s := range m.stores {
		if err := s.Upsert(ctx, id, vector, metadata); err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", s, err))
		}
	}
	return errors.Join(errs...)
}

// Close closes all stores, collecting errors from each.
func (m *MultiStore) Close() error {
	var errs []error
	for _, s := range m.stores {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
