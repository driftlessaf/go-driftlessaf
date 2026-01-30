/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"google.golang.org/genai"
)

// google implements Interface using Google Gemini
type google struct {
	goldenExecutor     googleexecutor.Interface[*Request, *Judgement]
	benchmarkExecutor  googleexecutor.Interface[*Request, *Judgement]
	standaloneExecutor googleexecutor.Interface[*Request, *Judgement]
}

// newGoogle creates a new Google Gemini judge instance
func newGoogle(ctx context.Context, projectID, region, model string, opts ...googleexecutor.Option[*Request, *Judgement]) (Interface, error) {
	// Create the Google AI client
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: region,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Google AI client: %w", err)
	}

	// Create response schema for structured JSON output
	responseSchema := &genai.Schema{
		Type: "object",
		Properties: map[string]*genai.Schema{
			"mode": {
				Type:        "string",
				Description: "The judgment mode: golden, benchmark, or standalone",
			},
			"score": {
				Type:        "number",
				Description: "The evaluation score",
			},
			"reasoning": {
				Type:        "string",
				Description: "Explanation of the score",
			},
			"suggestions": {
				Type: "array",
				Items: &genai.Schema{
					Type:        "string",
					Description: "Improvement suggestions",
				},
			},
		},
		Required: []string{"mode", "score", "reasoning", "suggestions"},
	}

	// Create golden mode executor
	goldenOpts := []googleexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		googleexecutor.WithModel[*Request, *Judgement](model),
		googleexecutor.WithTemperature[*Request, *Judgement](0.1), // Lower temperature for consistent judgments
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](8192),
		googleexecutor.WithResponseMIMEType[*Request, *Judgement]("application/json"),
		googleexecutor.WithResponseSchema[*Request, *Judgement](responseSchema),
	}
	goldenOpts = append(goldenOpts, opts...) // Apply caller-provided options (e.g., enricher)
	goldenExecutor, err := googleexecutor.New[*Request, *Judgement](
		client,
		goldenPrompt,
		goldenOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create golden executor: %w", err)
	}

	// Create benchmark mode executor
	benchmarkOpts := []googleexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		googleexecutor.WithModel[*Request, *Judgement](model),
		googleexecutor.WithTemperature[*Request, *Judgement](0.1), // Lower temperature for consistent judgments
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](8192),
		googleexecutor.WithResponseMIMEType[*Request, *Judgement]("application/json"),
		googleexecutor.WithResponseSchema[*Request, *Judgement](responseSchema),
	}
	benchmarkOpts = append(benchmarkOpts, opts...) // Apply caller-provided options (e.g., enricher)
	benchmarkExecutor, err := googleexecutor.New[*Request, *Judgement](
		client,
		benchmarkPrompt,
		benchmarkOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create benchmark executor: %w", err)
	}

	// Create standalone mode executor
	standaloneOpts := []googleexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		googleexecutor.WithModel[*Request, *Judgement](model),
		googleexecutor.WithTemperature[*Request, *Judgement](0.1), // Lower temperature for consistent judgments
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](8192),
		googleexecutor.WithResponseMIMEType[*Request, *Judgement]("application/json"),
		googleexecutor.WithResponseSchema[*Request, *Judgement](responseSchema),
	}
	standaloneOpts = append(standaloneOpts, opts...) // Apply caller-provided options (e.g., enricher)
	standaloneExecutor, err := googleexecutor.New[*Request, *Judgement](
		client,
		standalonePrompt, // Use pre-parsed template from prompts.go
		standaloneOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create standalone executor: %w", err)
	}

	return &google{
		goldenExecutor:     goldenExecutor,
		benchmarkExecutor:  benchmarkExecutor,
		standaloneExecutor: standaloneExecutor,
	}, nil
}

// Judge implements Interface
func (g *google) Judge(ctx context.Context, request *Request) (*Judgement, error) {
	// Validate request and select executor based on mode
	var executor googleexecutor.Interface[*Request, *Judgement]

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
		executor = g.goldenExecutor

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
		executor = g.benchmarkExecutor

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
		executor = g.standaloneExecutor

	default:
		return nil, fmt.Errorf("unsupported mode: %q", request.Mode)
	}

	// Execute with selected executor
	return executor.Execute(ctx, request, nil)
}
