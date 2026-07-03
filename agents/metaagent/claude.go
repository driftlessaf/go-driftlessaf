/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

// claudeAgent implements Agent using Claude via Vertex AI (default) or the
// Anthropic-direct first-party API + WIF backend when configured (see
// anthropicauth).
type claudeAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor claudeexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
	// validateTool is the non-terminal companion to submit_result. It is merged
	// into the tool set on each Execute so the model can check a payload's shape
	// without ending the run. Zero value (nil Handler) when unavailable.
	validateTool claudetool.Metadata[Resp]
}

func newClaudeAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	// Backend selection is env-driven: ANY binary embedding this package —
	// production reconcilers included, not just evals — switches from Vertex to
	// Anthropic-direct when ANTHROPIC_FEDERATION_RULE_ID and
	// ANTHROPIC_IDENTITY_TOKEN_FILE are present in its environment. That is the
	// intended per-deployment rollout lever (DEV-1839); anthropicauth logs which
	// backend it picked.
	authCfg := anthropicauth.ConfigFromEnv()
	client := anthropicauth.NewClient(ctx, projectID, region, authCfg)
	if authCfg.Configured() {
		// The first-party API rejects Vertex-style "name@version" model IDs.
		model = anthropicauth.ModelID(model)
	}

	// Build the terminal submit_result tool and its non-terminal validate
	// companion together so they share an identical schema and submit_result's
	// payload errors point the model at validate_result.
	submitTool, validateTool, err := submitresult.ClaudeSubmitAndValidateForResponse[Resp]()
	if err != nil {
		return nil, fmt.Errorf("building submit/validate tools: %w", err)
	}

	executorOpts := []claudeexecutor.Option[Req, Resp]{
		claudeexecutor.WithModel[Req, Resp](model),
		claudeexecutor.WithTemperature[Req, Resp](0.2),
		claudeexecutor.WithMaxTokens[Req, Resp](32000),
		claudeexecutor.WithSubmitResultProvider[Req, Resp](func() (claudetool.Metadata[Resp], error) { return submitTool, nil }),
		claudeexecutor.WithResourceLabels[Req, Resp](map[string]string{"projectID": projectID, "region": region, "model_name": strings.ToLower(model)}),
	}

	if config.MaxTurns > 0 {
		executorOpts = append(executorOpts, claudeexecutor.WithMaxTurns[Req, Resp](config.MaxTurns))
	}

	if config.ToolCallConcurrency > 0 {
		executorOpts = append(executorOpts, claudeexecutor.WithToolCallConcurrency[Req, Resp](config.ToolCallConcurrency))
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, claudeexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
	}

	if config.ThinkingBudget > 0 {
		executorOpts = append(executorOpts, claudeexecutor.WithThinking[Req, Resp](config.ThinkingBudget))
	}

	executor, err := claudeexecutor.New[Req, Resp](client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Claude executor: %w", err)
	}

	return &claudeAgent[Req, Resp, CB]{
		executor:     executor,
		config:       config,
		validateTool: validateTool,
	}, nil
}

func (a *claudeAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	claudeTools := claudetool.Map(tools)
	if a.validateTool.Handler != nil {
		if _, exists := claudeTools[a.validateTool.Definition.Name]; !exists {
			claudeTools[a.validateTool.Definition.Name] = a.validateTool
		}
	}
	return a.executor.Execute(ctx, request, claudeTools)
}
