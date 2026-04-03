/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package rag

import (
	"context"
	"fmt"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/option"
)

// minNeighborCount is the minimum number of neighbors requested from the API
// to ensure reasonable result quality when TopK is small.
const minNeighborCount = 10

// Retriever searches a vector index for similar embeddings.
type Retriever interface {
	// Search finds the nearest neighbors to the query vector.
	// Results are ordered by distance (most similar first).
	// Returns an empty (non-nil) slice when no results match.
	Search(ctx context.Context, query []float32, opts SearchOptions) ([]Result, error)

	// Close releases resources held by the retriever.
	Close() error
}

// Compile-time interface assertion.
var _ Retriever = (*MatchingEngineRetriever)(nil)

// MatchingEngineRetriever implements Retriever using the Vertex AI MatchClient
// gRPC API. It connects directly to the index endpoint's public domain for
// FindNeighbors calls.
type MatchingEngineRetriever struct {
	client          *aiplatform.MatchClient
	indexEndpoint   string // full resource name: projects/{p}/locations/{l}/indexEndpoints/{id}
	deployedIndexID string
}

// NewMatchingEngineRetriever creates a retriever backed by a deployed Vertex AI
// Matching Engine index using gRPC.
//
// Parameters:
//   - project, location: GCP project and region
//   - indexEndpointID: the index endpoint resource ID
//   - deployedIndexID: the ID of the deployed index within the endpoint
//   - publicDomainName: the public endpoint domain (e.g., "1234.us-central1-5678.vdb.vertexai.goog").
//     Required for public endpoints. For private (VPC) endpoints, pass "".
func NewMatchingEngineRetriever(ctx context.Context, project, location, indexEndpointID, deployedIndexID, publicDomainName string) (*MatchingEngineRetriever, error) {
	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if location == "" {
		return nil, fmt.Errorf("location is required")
	}
	if indexEndpointID == "" {
		return nil, fmt.Errorf("indexEndpointID is required")
	}
	if deployedIndexID == "" {
		return nil, fmt.Errorf("deployedIndexID is required")
	}

	// The MatchClient defaults to aiplatform.googleapis.com:443 which does NOT
	// serve FindNeighbors. For public endpoints we must connect to the index
	// endpoint's own domain. For VPC endpoints the regional endpoint works.
	endpoint := fmt.Sprintf("%s-aiplatform.googleapis.com:443", location)
	if publicDomainName != "" {
		endpoint = publicDomainName + ":443"
	}

	client, err := aiplatform.NewMatchClient(ctx,
		option.WithEndpoint(endpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("creating match client: %w", err)
	}

	indexEndpoint := fmt.Sprintf("projects/%s/locations/%s/indexEndpoints/%s",
		project, location, indexEndpointID)

	return &MatchingEngineRetriever{
		client:          client,
		indexEndpoint:   indexEndpoint,
		deployedIndexID: deployedIndexID,
	}, nil
}

// Search finds vectors similar to the query vector using gRPC FindNeighbors.
func (r *MatchingEngineRetriever) Search(ctx context.Context, query []float32, opts SearchOptions) ([]Result, error) {
	opts = opts.defaults()

	// Request more neighbors than TopK to leave room for threshold filtering.
	neighborCount := max(opts.TopK*2, minNeighborCount)

	resp, err := r.client.FindNeighbors(ctx, &aiplatformpb.FindNeighborsRequest{
		IndexEndpoint:   r.indexEndpoint,
		DeployedIndexId: r.deployedIndexID,
		Queries: []*aiplatformpb.FindNeighborsRequest_Query{{
			Datapoint: &aiplatformpb.IndexDatapoint{
				DatapointId:   "query",
				FeatureVector: query,
			},
			NeighborCount: int32(neighborCount),
		}},
		ReturnFullDatapoint: true,
	})
	if err != nil {
		return nil, fmt.Errorf("FindNeighbors: %w", err)
	}

	if len(resp.NearestNeighbors) == 0 || len(resp.NearestNeighbors[0].Neighbors) == 0 {
		return []Result{}, nil
	}

	neighbors := resp.NearestNeighbors[0].Neighbors
	results := make([]Result, 0, len(neighbors))
	for _, n := range neighbors {
		distance := float64(n.Distance)

		// DistanceThreshold <= 0 means no filtering (return all TopK).
		if opts.DistanceThreshold > 0 && distance > opts.DistanceThreshold {
			continue
		}

		md := make(map[string]string, len(n.Datapoint.GetEmbeddingMetadata().GetFields()))
		for k, v := range n.Datapoint.GetEmbeddingMetadata().GetFields() {
			if s := v.GetStringValue(); s != "" {
				md[k] = s
			} else {
				// Convert non-string values rather than silently dropping them.
				md[k] = fmt.Sprintf("%v", v.AsInterface())
			}
		}

		results = append(results, Result{
			ID:       n.Datapoint.DatapointId,
			Distance: distance,
			Metadata: md,
		})

		if len(results) >= opts.TopK {
			break
		}
	}

	return results, nil
}

// Close releases the gRPC connection.
func (r *MatchingEngineRetriever) Close() error {
	return r.client.Close()
}
