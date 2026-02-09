/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"

	"chainguard.dev/driftlessaf/agents/metaagent"
)

// newPRFixerAgent creates a new meta-agent for PR fixing.
// The agent supports both Claude and Gemini models based on the model name prefix.
func newPRFixerAgent(ctx context.Context, cfg *config) (metaagent.Agent[*PRContext, *PRFixResult, PRTools], error) {
	return metaagent.New[*PRContext, *PRFixResult, PRTools]( //nolint:infertypeargs // Req cannot be inferred from Config
		ctx, cfg.GCPProjectID, cfg.GCPRegion, cfg.Model,
		metaagent.Config[*PRFixResult, PRTools]{
			SystemInstructions: systemInstructions,
			UserPrompt:         userPrompt,
			Tools:              NewPRToolsProvider(),
		},
	)
}
