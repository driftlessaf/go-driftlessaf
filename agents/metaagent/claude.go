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

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/internal/claudebackend"
	agentmodel "chainguard.dev/driftlessaf/agents/model"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// defaultMaxTokens is the per-turn output-token cap applied when Config.MaxTokens
// is unset. It matches the historical meta-agent default; agents that need room
// for extended thinking plus a tool call on the same turn raise it via Config.
const defaultMaxTokens int64 = 32000

// defaultClaudeTemperature is the meta-agent's sampling temperature on models
// whose provider surface accepts sampling parameters.
const defaultClaudeTemperature = 0.2

// claudeAgent implements Agent using the Claude backend selected by
// claudebackend.
type claudeAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor claudeexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
}

func newClaudeAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	backend, err := claudebackend.Resolve(ctx, projectID, region, model)
	if err != nil {
		return nil, err
	}

	// Build the terminal submit_result tool. The executor gates accepted
	// submissions on the configured result validators before committing them
	// as the run's final result.
	submitTool, err := submitresult.ClaudeTool(submitOptions(config))
	if err != nil {
		return nil, fmt.Errorf("building submit tool: %w", err)
	}

	executorOpts := []claudeexecutor.Option[Req, Resp]{
		claudeexecutor.WithModel[Req, Resp](backend.ModelID),
		claudeexecutor.WithProvider[Req, Resp](backend.Provider),
		claudeexecutor.WithMaxTokens[Req, Resp](cmp.Or(config.MaxTokens, defaultMaxTokens)),
		claudeexecutor.WithSubmitResultProvider[Req, Resp](func() (claudetool.SubmitMetadata[Resp], error) { return submitTool, nil }),
		claudeexecutor.WithResourceLabels[Req, Resp](map[string]string{"projectID": projectID, "region": region, "model_name": strings.ToLower(backend.ModelID)}),
	}
	executorOpts = append(executorOpts, claudeTemperatureOptions[Req, Resp](backend.ModelID)...)
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

	if config.RefusalNudgeMaxRetries > 0 {
		executorOpts = append(executorOpts, claudeexecutor.WithRefusalNudge[Req, Resp](config.RefusalNudgeMaxRetries))
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

	executor, err := claudeexecutor.NewWithMessages[Req, Resp](backend.Messages, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Claude executor: %w", err)
	}

	return &claudeAgent[Req, Resp, CB]{
		executor: executor,
		config:   config,
	}, nil
}

// claudeTemperatureOptions preserves the meta-agent's sampling behavior on
// models that accept temperature while leaving adaptive-thinking models free
// of a parameter their API rejects.
func claudeTemperatureOptions[Req promptbuilder.Bindable, Resp any](modelID string) []claudeexecutor.Option[Req, Resp] {
	if !agentmodel.Resolve(modelID).SamplingParams {
		return nil
	}
	return []claudeexecutor.Option[Req, Resp]{
		claudeexecutor.WithTemperature[Req, Resp](defaultClaudeTemperature),
	}
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
// type assertion; the executor built by claudeexecutor.NewWithMessages always
// satisfies it, making the ok-check a guard against a non-resumable Interface
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
