/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
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

	// Create one executor per mode using the pre-parsed templates from
	// prompts.go; executors apply options read-only, so one slice is shared.
	execOpts := []googleexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		googleexecutor.WithModel[*Request, *Judgement](model),
		googleexecutor.WithTemperature[*Request, *Judgement](0.1), // Lower temperature for consistent judgments
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](8192),
		googleexecutor.WithResponseMIMEType[*Request, *Judgement]("application/json"),
		googleexecutor.WithResponseSchema[*Request, *Judgement](responseSchema),
	}
	execOpts = append(execOpts, opts...) // Apply caller-provided options (e.g., enricher)
	executors := make([]googleexecutor.Interface[*Request, *Judgement], len(modePrompts))
	for i, mp := range modePrompts {
		executor, err := googleexecutor.New[*Request, *Judgement](
			client,
			mp.prompt,
			execOpts...,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create %s executor: %w", mp.name, err)
		}
		executors[i] = executor
	}

	return &google{
		goldenExecutor:     executors[0],
		benchmarkExecutor:  executors[1],
		standaloneExecutor: executors[2],
	}, nil
}

// Judge implements Interface
func (g *google) Judge(ctx context.Context, request *Request) (*Judgement, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}

	// Select executor based on mode
	var executor googleexecutor.Interface[*Request, *Judgement]
	switch request.Mode {
	case GoldenMode:
		executor = g.goldenExecutor
	case BenchmarkMode:
		executor = g.benchmarkExecutor
	case StandaloneMode:
		executor = g.standaloneExecutor
	default:
		return nil, fmt.Errorf("unsupported mode: %q", request.Mode)
	}

	// Stamp the agent name so the executor-layer trace carries
	// gen_ai.agent.name=judge on its root invoke_agent span. See
	// agenttrace.WithDefaultAgentName.
	ctx = agenttrace.WithDefaultAgentName(ctx, "judge")

	// Execute with selected executor
	return executor.Execute(ctx, request, nil)
}
