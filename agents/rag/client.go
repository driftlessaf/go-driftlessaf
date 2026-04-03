/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package rag

import (
	"context"
	"errors"
	"fmt"
	"maps"
)

// ClientConfig configures a RAG Client with all three components.
type ClientConfig struct {
	// GCP project ID.
	Project string

	// GCP region (e.g., "us-east5").
	Location string

	// Embedding model name (e.g., "gemini-embedding-001").
	EmbeddingModel string

	// Vertex AI Matching Engine index resource name.
	// Format: projects/{project}/locations/{location}/indexes/{index}
	IndexName string

	// Vertex AI index endpoint resource ID.
	IndexEndpointID string

	// Deployed index ID within the endpoint.
	DeployedIndexID string

	// PublicDomainName is the public endpoint domain for the index endpoint
	// (e.g., "1234.us-central1-5678.vdb.vertexai.goog").
	// Required for public endpoints. Leave empty for private (VPC) endpoints.
	PublicDomainName string

	// GCSBucket enables durable persistence of embeddings to GCS.
	// When set, stores write to both GCS and Matching Engine.
	// Records in GCS can be re-embedded when upgrading models.
	// Optional — leave empty for Matching Engine only.
	GCSBucket string

	// GCSPrefix is an optional path prefix within the GCS bucket.
	GCSPrefix string
}

// Client wraps Embedder, Store, and Retriever for common RAG workflows.
type Client struct {
	embedder  *Embedder
	store     Store
	retriever Retriever
}

// Embedder returns the client's embedder for direct use.
func (c *Client) Embedder() *Embedder { return c.embedder }

// Store returns the client's store for direct use.
func (c *Client) Store() Store { return c.store }

// Retriever returns the client's retriever for direct use.
func (c *Client) Retriever() Retriever { return c.retriever }

// NewClient creates a fully configured RAG client with embedding, storage, and retrieval.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	embedder, err := NewEmbedder(ctx, cfg.Project, cfg.Location, cfg.EmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("creating embedder: %w", err)
	}

	meStore, err := NewMatchingEngineStore(ctx, cfg.Location, cfg.IndexName)
	if err != nil {
		embedder.Close()
		return nil, fmt.Errorf("creating store: %w", err)
	}

	// If GCS bucket is configured, wrap with MultiStore for durability.
	var store Store = meStore
	if cfg.GCSBucket != "" {
		gcsStore, err := NewGCSStore(ctx, cfg.GCSBucket, cfg.GCSPrefix)
		if err != nil {
			embedder.Close()
			meStore.Close()
			return nil, fmt.Errorf("creating GCS store: %w", err)
		}
		store = NewMultiStore(gcsStore, meStore)
	}

	retriever, err := NewMatchingEngineRetriever(ctx, cfg.Project, cfg.Location, cfg.IndexEndpointID, cfg.DeployedIndexID, cfg.PublicDomainName)
	if err != nil {
		embedder.Close()
		store.Close()
		return nil, fmt.Errorf("creating retriever: %w", err)
	}

	return &Client{
		embedder:  embedder,
		store:     store,
		retriever: retriever,
	}, nil
}

// EmbedAndStore generates an embedding for the text and stores it with the given metadata.
// The source text is automatically included in metadata under MetadataKeySourceText
// so that embeddings can be regenerated when upgrading models.
//
// The caller's metadata map is not modified; a copy is made before adding the source text.
func (c *Client) EmbedAndStore(ctx context.Context, id, text string, taskType TaskType, metadata map[string]string) error {
	return c.embedAndStore(ctx, id, text, taskType, metadata, c.embedder.Embed)
}

// embedFn is the signature for generating an embedding vector from text.
type embedFn func(ctx context.Context, text string, taskType TaskType) ([]float32, error)

// embedAndStore is the core implementation, accepting an embed function for testability.
func (c *Client) embedAndStore(ctx context.Context, id, text string, taskType TaskType, metadata map[string]string, embed embedFn) error {
	vector, err := embed(ctx, text, taskType)
	if err != nil {
		return fmt.Errorf("embedding text: %w", err)
	}

	// Clone metadata to avoid mutating the caller's map.
	md := make(map[string]string, len(metadata)+1)
	maps.Copy(md, metadata)
	// Preserve source text for re-embedding with future models.
	if _, ok := md[MetadataKeySourceText]; !ok {
		md[MetadataKeySourceText] = text
	}

	if err := c.store.Upsert(ctx, id, vector, md); err != nil {
		return fmt.Errorf("storing embedding: %w", err)
	}

	return nil
}

// EmbedAndSearch generates an embedding for the query text and searches for similar vectors.
// Queries are embedded with TaskTypeRetrievalQuery, the counterpart to
// TaskTypeRetrievalDocument used during ingestion.
func (c *Client) EmbedAndSearch(ctx context.Context, queryText string, opts SearchOptions) ([]Result, error) {
	vector, err := c.embedder.Embed(ctx, queryText, TaskTypeRetrievalQuery)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	results, err := c.retriever.Search(ctx, vector, opts)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}

	return results, nil
}

// Close releases all resources held by the client.
func (c *Client) Close() error {
	return errors.Join(c.embedder.Close(), c.store.Close(), c.retriever.Close())
}
