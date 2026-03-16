/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"

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

	executorOpts := []googleexecutor.Option[Req, Resp]{
		googleexecutor.WithModel[Req, Resp](model),
		googleexecutor.WithTemperature[Req, Resp](0.2),
		googleexecutor.WithMaxOutputTokens[Req, Resp](32768),
		googleexecutor.WithSubmitResultProvider[Req, Resp](submitresult.GoogleToolForResponse[Resp]),
		googleexecutor.WithResourceLabels[Req, Resp](map[string]string{"projectID": projectID, "region": region}),
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, googleexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
	}

	executor, err := googleexecutor.New[Req, Resp](client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Google executor: %w", err)
	}

	return &googleAgent[Req, Resp, CB]{
		executor: executor,
		config:   config,
	}, nil
}

func (a *googleAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	return a.executor.Execute(ctx, request, googletool.Map(tools))
}
