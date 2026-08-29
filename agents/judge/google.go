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
	"chainguard.dev/driftlessaf/agents/metaagent"
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
	return newGoogleWithClient(googleJudgeConstruction{
		client:          client,
		providerModelID: model,
		logicalModelID:  model,
		samplingParams:  true,
	}, opts...)
}

type googleJudgeConstruction struct {
	client          *genai.Client
	providerModelID string
	logicalModelID  string
	routed          bool
	samplingParams  bool
	resourceLabels  map[string]string
	attribution     *agenttrace.Attribution
}

func newRoutedGoogle(binding metaagent.GoogleGenAIBinding) (Interface, error) {
	plan := binding.Plan()
	attribution := routedAttribution(plan)
	return newGoogleWithClient(googleJudgeConstruction{
		client:          binding.Client(),
		providerModelID: plan.ProviderModelID(),
		logicalModelID:  plan.LogicalModel(),
		routed:          true,
		samplingParams:  plan.Capabilities().SamplingParameters,
		resourceLabels:  binding.ResourceLabels(),
		attribution:     &attribution,
	})
}

func newGoogleWithClient(construction googleJudgeConstruction, opts ...googleexecutor.Option[*Request, *Judgement]) (Interface, error) {
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
		googleexecutor.WithTemperature[*Request, *Judgement](0.1),
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](8192),
		googleexecutor.WithResponseMIMEType[*Request, *Judgement]("application/json"),
		googleexecutor.WithResponseSchema[*Request, *Judgement](responseSchema),
	}
	if construction.routed {
		execOpts = append(execOpts,
			googleexecutor.WithRoutedModel[*Request, *Judgement](construction.providerModelID, construction.logicalModelID),
			googleexecutor.WithAttribution[*Request, *Judgement](*construction.attribution),
			googleexecutor.WithResourceLabels[*Request, *Judgement](construction.resourceLabels),
		)
		if !construction.samplingParams {
			execOpts = append(execOpts, googleexecutor.WithoutTemperature[*Request, *Judgement]())
		}
	} else {
		execOpts = append([]googleexecutor.Option[*Request, *Judgement]{
			googleexecutor.WithModel[*Request, *Judgement](construction.providerModelID),
		}, execOpts...)
		execOpts = append(execOpts, opts...) // Preserve compatibility option precedence.
	}
	executors := make([]googleexecutor.Interface[*Request, *Judgement], len(modePrompts))
	for i, mp := range modePrompts {
		executor, err := googleexecutor.New[*Request, *Judgement](
			construction.client,
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
