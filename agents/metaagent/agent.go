/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"strings"

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
// The model parameter determines which provider implementation is used:
//   - Models starting with "gemini-" use Google's Generative AI SDK
//   - Models starting with "claude-" use Anthropic's SDK via Vertex AI
func New[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	modelLower := strings.ToLower(model)

	switch {
	case strings.HasPrefix(modelLower, "gemini-"):
		return newGoogleAgent[Req, Resp, CB](ctx, projectID, region, model, config)
	case strings.HasPrefix(modelLower, "claude-"):
		return newClaudeAgent[Req, Resp, CB](ctx, projectID, region, model, config)
	default:
		return nil, fmt.Errorf("unsupported model: %s (expected gemini-* or claude-*)", model)
	}
}
