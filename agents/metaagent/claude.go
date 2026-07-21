/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// defaultMaxTokens is the per-turn output-token cap applied when Config.MaxTokens
// is unset. It matches the historical meta-agent default; agents that need room
// for extended thinking plus a tool call on the same turn raise it via Config.
const defaultMaxTokens int64 = 32000

// claudeAgent implements Agent using Claude via Vertex AI (default) or the
// Anthropic-direct first-party API + WIF backend when configured (see
// anthropicauth).
type claudeAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor claudeexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
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
	authCfg, err := anthropicauth.ConfigFromEnv()
	if err != nil {
		// A named-but-broken profile is a deploy error; failing here beats
		// silently serving the Vertex zero-value backend.
		return nil, fmt.Errorf("resolving anthropic auth config: %w", err)
	}
	client := anthropicauth.NewClient(ctx, projectID, region, authCfg)
	// Stamp the true serving backend on metrics + traces: the same Claude
	// model bills to GCP on Vertex and to the Anthropic workspace on the
	// first-party API, so stored telemetry must not infer the backend from
	// model-ID shape.
	provider := claudeexecutor.ProviderVertex
	if authCfg.Configured() {
		// The first-party API rejects Vertex-style "name@version" model IDs.
		model = anthropicauth.ModelID(model)
		provider = claudeexecutor.ProviderAnthropic
	}

	// Build the terminal submit_result tool. The executor gates accepted
	// submissions on the configured result validators before committing them
	// as the run's final result.
	submitTool, err := submitresult.ClaudeToolForResponse[Resp]()
	if err != nil {
		return nil, fmt.Errorf("building submit tool: %w", err)
	}

	executorOpts := []claudeexecutor.Option[Req, Resp]{
		claudeexecutor.WithModel[Req, Resp](model),
		claudeexecutor.WithProvider[Req, Resp](provider),
		claudeexecutor.WithTemperature[Req, Resp](0.2),
		claudeexecutor.WithMaxTokens[Req, Resp](cmp.Or(config.MaxTokens, defaultMaxTokens)),
		claudeexecutor.WithSubmitResultProvider[Req, Resp](func() (claudetool.SubmitMetadata[Resp], error) { return submitTool, nil }),
		claudeexecutor.WithResourceLabels[Req, Resp](map[string]string{"projectID": projectID, "region": region, "model_name": strings.ToLower(model)}),
	}
	for _, v := range config.ResultValidators {
		executorOpts = append(executorOpts, claudeexecutor.WithResultValidator[Req, Resp](v))
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

	if config.UserPromptSuffix != nil {
		executorOpts = append(executorOpts, claudeexecutor.WithUserPromptSuffix[Req, Resp](config.UserPromptSuffix))
	}

	if config.ThinkingBudget > 0 {
		executorOpts = append(executorOpts, claudeexecutor.WithThinking[Req, Resp](config.ThinkingBudget))
	}

	if config.Effort != "" {
		executorOpts = append(executorOpts, claudeexecutor.WithEffort[Req, Resp](config.Effort))
	}

	if config.SuspendToolName != "" {
		name, desc := config.SuspendToolName, config.SuspendToolDescription
		executorOpts = append(executorOpts, claudeexecutor.WithSuspendTool[Req, Resp](func() (anthropic.ToolParam, error) {
			return anthropic.ToolParam{
				Name:        name,
				Description: anthropic.String(desc),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type:       "object",
					Properties: map[string]any{suspendQuestionProperty: map[string]any{"type": "string"}},
				},
			}, nil
		}))
	}

	executor, err := claudeexecutor.New[Req, Resp](client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Claude executor: %w", err)
	}

	return &claudeAgent[Req, Resp, CB]{
		executor: executor,
		config:   config,
	}, nil
}

func (a *claudeAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	return a.executor.Execute(ctx, request, claudetool.Map(tools))
}

// Resume implements Resumer by delegating to the concrete Claude executor's
// resume capability. Resume is deliberately off the executor's exported
// Interface (see claudeexecutor.Resumer), so the concrete type is reached by
// type assertion; the executor built by claudeexecutor.New always satisfies
// it, making the ok-check a guard against a future non-resumable Interface
// implementation being injected, not an expected runtime path.
func (a *claudeAgent[Req, Resp, CB]) Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, callbacks CB) (Resp, error) {
	var zero Resp
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		return zero, fmt.Errorf("building tools: %w", err)
	}
	resumer, ok := a.executor.(claudeexecutor.Resumer[Req, Resp])
	if !ok {
		return zero, fmt.Errorf("claude executor does not support resume")
	}
	return resumer.Resume(ctx, env, answers, claudetool.Map(tools))
}
