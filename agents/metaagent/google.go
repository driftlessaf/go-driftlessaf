/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// googleAgent implements Agent using Google's Generative AI SDK.
type googleAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor googleexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
	// validateTool is the non-terminal companion to submit_result, merged into
	// the tool set on each Execute. Zero value (nil Handler) when unavailable.
	validateTool googletool.Metadata[Resp]
}

func newGoogleAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  projectID,
		Location: region,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Google AI client: %w", err)
	}

	submitTool, validateTool, err := submitresult.GoogleSubmitAndValidateForResponse[Resp]()
	if err != nil {
		return nil, fmt.Errorf("building submit/validate tools: %w", err)
	}

	executorOpts := []googleexecutor.Option[Req, Resp]{
		googleexecutor.WithModel[Req, Resp](model),
		googleexecutor.WithTemperature[Req, Resp](0.2),
		googleexecutor.WithMaxOutputTokens[Req, Resp](65536), // Gemini 2.5 Flash max output tokens
		googleexecutor.WithSubmitResultProvider[Req, Resp](func() (googletool.Metadata[Resp], error) { return submitTool, nil }),
		googleexecutor.WithResourceLabels[Req, Resp](map[string]string{"projectID": projectID, "region": region, "model_name": strings.ToLower(model)}),
	}

	if config.MaxTurns > 0 {
		executorOpts = append(executorOpts, googleexecutor.WithMaxTurns[Req, Resp](config.MaxTurns))
	}

	if config.ToolCallConcurrency > 0 {
		executorOpts = append(executorOpts, googleexecutor.WithToolCallConcurrency[Req, Resp](config.ToolCallConcurrency))
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, googleexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
	}

	executor, err := googleexecutor.New[Req, Resp](client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Google executor: %w", err)
	}

	return &googleAgent[Req, Resp, CB]{
		executor:     executor,
		config:       config,
		validateTool: validateTool,
	}, nil
}

func (a *googleAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	googleTools := googletool.Map(tools)
	if a.validateTool.Handler != nil && a.validateTool.Definition != nil {
		if _, exists := googleTools[a.validateTool.Definition.Name]; !exists {
			googleTools[a.validateTool.Definition.Name] = a.validateTool
		}
	}
	return a.executor.Execute(ctx, request, googleTools)
}
