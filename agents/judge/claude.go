/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/internal/claudebackend"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"github.com/anthropics/anthropic-sdk-go"
)

// claude implements Interface using the backend selected by claudebackend.
type claude struct {
	goldenExecutor     claudeexecutor.Interface[*Request, *Judgement]
	benchmarkExecutor  claudeexecutor.Interface[*Request, *Judgement]
	standaloneExecutor claudeexecutor.Interface[*Request, *Judgement]
}

// newClaude creates a new Claude judge instance
func newClaude(ctx context.Context, projectID, region, model string, opts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
	backend, err := claudebackend.Resolve(ctx, projectID, region, model)
	if err != nil {
		return nil, err
	}
	provider := backend.Provider
	return newClaudeWithMessages(claudeJudgeConstruction{
		messages:        backend.Messages,
		providerModelID: backend.ModelID,
		logicalModelID:  backend.ModelID,
		legacyProvider:  &provider,
		samplingParams:  true,
	}, opts...)
}

type claudeJudgeConstruction struct {
	messages        anthropic.MessageService
	providerModelID string
	logicalModelID  string
	legacyProvider  *claudeexecutor.Provider
	routed          bool
	samplingParams  bool
	promptCaching   bool
	resourceLabels  map[string]string
	attribution     *agenttrace.Attribution
}

func newRoutedClaude(binding metaagent.AnthropicMessagesBinding) (Interface, error) {
	plan := binding.Plan()
	attribution := routedAttribution(plan)
	return newClaudeWithMessages(claudeJudgeConstruction{
		messages:        binding.Messages(),
		providerModelID: plan.ProviderModelID(),
		logicalModelID:  plan.LogicalModel(),
		routed:          true,
		samplingParams:  plan.Capabilities().SamplingParameters,
		promptCaching:   plan.Capabilities().PromptCaching,
		resourceLabels:  binding.ResourceLabels(),
		attribution:     &attribution,
	})
}

func newClaudeWithMessages(construction claudeJudgeConstruction, opts ...claudeexecutor.Option[*Request, *Judgement]) (Interface, error) {
	// Create one executor per mode using the pre-parsed templates from
	// prompts.go; executors apply options read-only, so one slice is shared.
	execOpts := []claudeexecutor.Option[*Request, *Judgement]{ //nolint: prealloc
		claudeexecutor.WithMaxTokens[*Request, *Judgement](8192),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.1),
	}
	if construction.routed {
		execOpts = append(execOpts,
			claudeexecutor.WithRoutedModel[*Request, *Judgement](construction.providerModelID, construction.logicalModelID),
			claudeexecutor.WithAttribution[*Request, *Judgement](*construction.attribution),
			claudeexecutor.WithResourceLabels[*Request, *Judgement](construction.resourceLabels),
		)
		if !construction.samplingParams {
			execOpts = append(execOpts, claudeexecutor.WithoutTemperature[*Request, *Judgement]())
		}
		if !construction.promptCaching {
			execOpts = append(execOpts, claudeexecutor.WithoutCacheControl[*Request, *Judgement]())
		}
	} else {
		execOpts = append([]claudeexecutor.Option[*Request, *Judgement]{
			claudeexecutor.WithModel[*Request, *Judgement](construction.providerModelID),
			claudeexecutor.WithProvider[*Request, *Judgement](*construction.legacyProvider),
		}, execOpts...)
		execOpts = append(execOpts, opts...) // Preserve compatibility option precedence.
	}
	executors := make([]claudeexecutor.Interface[*Request, *Judgement], len(modePrompts))
	for i, mp := range modePrompts {
		executor, err := claudeexecutor.NewWithMessages[*Request, *Judgement](
			construction.messages,
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
