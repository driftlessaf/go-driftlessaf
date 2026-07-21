/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/model"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// Agent is the interface for a configured meta-agent.
//   - Req must implement promptbuilder.Bindable.
//   - Resp is the structured response type.
//   - CB is the type providing all tool callbacks.
type Agent[Req promptbuilder.Bindable, Resp, CB any] interface {
	// Execute runs the agent with the given request and tool callbacks.
	Execute(ctx context.Context, request Req, callbacks CB) (Resp, error)
}

// New creates a new meta-agent with the given configuration.
// The modelName parameter determines which provider implementation is used:
//   - Models starting with "gemini-" use Google's Generative AI SDK (native)
//   - Models starting with "claude-" use Anthropic's SDK via Vertex AI (native)
//   - Models in "publisher/model" format use Vertex AI's OpenAI-compatible endpoint
func New[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, modelName string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	switch model.Resolve(modelName).Backend {
	case model.BackendGemini:
		return newGoogleAgent[Req, Resp, CB](ctx, projectID, region, modelName, config)
	case model.BackendClaude:
		return newClaudeAgent[Req, Resp, CB](ctx, projectID, region, modelName, config)
	case model.BackendOpenAICompat:
		// publisher/model format routes to the Vertex AI OpenAI-compatible endpoint
		return newOpenAICompatAgent[Req, Resp, CB](ctx, projectID, region, modelName, config)
	default:
		return nil, fmt.Errorf("unsupported model: %s (expected gemini-*, claude-*, or publisher/model format)", modelName)
	}
}
