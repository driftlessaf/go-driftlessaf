/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// claudeAgent implements Agent using Claude via Vertex AI.
type claudeAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor claudeexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
}

func newClaudeAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	client := anthropic.NewClient(
		vertex.WithGoogleAuth(ctx, region, projectID),
	)

	executorOpts := []claudeexecutor.Option[Req, Resp]{
		claudeexecutor.WithModel[Req, Resp](model),
		claudeexecutor.WithTemperature[Req, Resp](0.2),
		claudeexecutor.WithMaxTokens[Req, Resp](32000),
		claudeexecutor.WithSubmitResultProvider[Req, Resp](submitresult.ClaudeToolForResponse[Resp]),
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, claudeexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
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
	tools := claudetool.Map(a.config.Tools.Tools(callbacks))
	return a.executor.Execute(ctx, request, tools)
}
