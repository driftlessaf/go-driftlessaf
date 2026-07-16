/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
)

// claude implements Interface using Claude via Vertex AI (default) or the
// Anthropic-direct first-party API + WIF backend when configured (see
// anthropicauth).
type claude struct {
	goldenExecutor     claudeexecutor.Interface[*Request, *Judgement]
	benchmarkExecutor  claudeexecutor.Interface[*Request, *Judgement]
	standaloneExecutor claudeexecutor.Interface[*Request, *Judgement]
}

// newClaude creates a new Claude judge instance
func newClaude(ctx context.Context, projectID, region, model string, opts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
	// Create client: Vertex AI by default, or the Anthropic-direct first-party
	// API + WIF backend when configured (see anthropicauth). Selection is
	// env-driven: ANY binary embedding this package — production reconcilers
	// included, not just evals — switches to Anthropic-direct when
	// ANTHROPIC_FEDERATION_RULE_ID and ANTHROPIC_IDENTITY_TOKEN_FILE are present
	// in its environment. That is the intended per-deployment rollout lever
	// (DEV-1839); anthropicauth logs which backend it picked.
	authCfg, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		// A named-but-broken profile is a deploy error; failing here beats
		// silently serving the Vertex zero-value backend.
		return nil, fmt.Errorf("resolving anthropic auth config: %w", err)
	}
	client := anthropicauth.NewClient(ctx, projectID, region, authCfg)
	// Stamp the true serving backend on metrics + traces (see
	// claudeexecutor.Provider): Vertex and the first-party API bill
	// differently for the same model.
	provider := claudeexecutor.ProviderVertex
	if authCfg.Configured() {
		// The first-party API rejects Vertex-style "name@version" model IDs.
		model = anthropicauth.ModelID(model)
		provider = claudeexecutor.ProviderAnthropic
	}

	// Create one executor per mode using the pre-parsed templates from
	// prompts.go; executors apply options read-only, so one slice is shared.
	execOpts := []claudeexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		claudeexecutor.WithModel[*Request, *Judgement](model),
		claudeexecutor.WithProvider[*Request, *Judgement](provider),
		claudeexecutor.WithMaxTokens[*Request, *Judgement](8192),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.1),
	}
	execOpts = append(execOpts, opts...) // Apply caller-provided options (e.g., enricher)
	executors := make([]claudeexecutor.Interface[*Request, *Judgement], len(modePrompts))
	for i, mp := range modePrompts {
		executor, err := claudeexecutor.New[*Request, *Judgement](
			client,
			mp.prompt,
			execOpts...,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create %s executor: %w", mp.name, err)
		}
		executors[i] = executor
	}

	return &claude{
		goldenExecutor:     executors[0],
		benchmarkExecutor:  executors[1],
		standaloneExecutor: executors[2],
	}, nil
}

// Judge implements Interface
func (c *claude) Judge(ctx context.Context, request *Request) (*Judgement, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}

	// Select executor based on mode
	var executor claudeexecutor.Interface[*Request, *Judgement]
	switch request.Mode {
	case GoldenMode:
		executor = c.goldenExecutor
	case BenchmarkMode:
		executor = c.benchmarkExecutor
	case StandaloneMode:
		executor = c.standaloneExecutor
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
