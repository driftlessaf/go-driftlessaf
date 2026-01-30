/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// claude implements Interface using Claude via Vertex AI
type claude struct {
	goldenExecutor     claudeexecutor.Interface[*Request, *Judgement]
	benchmarkExecutor  claudeexecutor.Interface[*Request, *Judgement]
	standaloneExecutor claudeexecutor.Interface[*Request, *Judgement]
}

// newClaude creates a new Claude judge instance
func newClaude(ctx context.Context, projectID, region, model string, opts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
	// Create client with Vertex AI authentication
	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID),
	)

	// Use pre-parsed templates from prompts.go

	// Create golden executor
	goldenOpts := []claudeexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		claudeexecutor.WithModel[*Request, *Judgement](model),
		claudeexecutor.WithMaxTokens[*Request, *Judgement](8192),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.1),
	}
	goldenOpts = append(goldenOpts, opts...) // Apply caller-provided options (e.g., enricher)
	goldenExecutor, err := claudeexecutor.New[*Request, *Judgement](
		client,
		goldenPrompt,
		goldenOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create golden executor: %w", err)
	}

	// Create benchmark executor
	benchmarkOpts := []claudeexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		claudeexecutor.WithModel[*Request, *Judgement](model),
		claudeexecutor.WithMaxTokens[*Request, *Judgement](8192),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.1),
	}
	benchmarkOpts = append(benchmarkOpts, opts...) // Apply caller-provided options (e.g., enricher)
	benchmarkExecutor, err := claudeexecutor.New[*Request, *Judgement](
		client,
		benchmarkPrompt,
		benchmarkOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create benchmark executor: %w", err)
	}

	// Create standalone executor
	standaloneOpts := []claudeexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		claudeexecutor.WithModel[*Request, *Judgement](model),
		claudeexecutor.WithMaxTokens[*Request, *Judgement](8192),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.1),
	}
	standaloneOpts = append(standaloneOpts, opts...) // Apply caller-provided options (e.g., enricher)
	standaloneExecutor, err := claudeexecutor.New[*Request, *Judgement](
		client,
		standalonePrompt,
		standaloneOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create standalone executor: %w", err)
	}

	return &claude{
		goldenExecutor:     goldenExecutor,
		benchmarkExecutor:  benchmarkExecutor,
		standaloneExecutor: standaloneExecutor,
	}, nil
}

// Judge implements Interface
func (c *claude) Judge(ctx context.Context, request *Request) (*Judgement, error) {
	// Validate request and select executor based on mode
	var executor claudeexecutor.Interface[*Request, *Judgement]

	switch request.Mode {
	case GoldenMode:
		if request.ReferenceAnswer == "" {
			return nil, errors.New("reference_answer is required for golden mode")
		}
		if request.ActualAnswer == "" {
			return nil, errors.New("actual_answer is required")
		}
		if request.Criterion == "" {
			return nil, errors.New("criterion is required")
		}
		executor = c.goldenExecutor

	case BenchmarkMode:
		if request.ReferenceAnswer == "" {
			return nil, errors.New("reference_answer (first candidate) is required for benchmark mode")
		}
		if request.ActualAnswer == "" {
			return nil, errors.New("actual_answer (second candidate) is required for benchmark mode")
		}
		if request.Criterion == "" {
			return nil, errors.New("criterion is required for benchmark mode")
		}
		executor = c.benchmarkExecutor

	case StandaloneMode:
		if request.ReferenceAnswer != "" {
			return nil, errors.New("reference_answer must not be provided for standalone mode")
		}
		if request.ActualAnswer == "" {
			return nil, errors.New("actual_answer is required for standalone mode")
		}
		if request.Criterion == "" {
			return nil, errors.New("criterion is required for standalone mode")
		}
		executor = c.standaloneExecutor

	default:
		return nil, fmt.Errorf("unsupported mode: %q", request.Mode)
	}

	// Execute with selected executor
	return executor.Execute(ctx, request, nil)
}
