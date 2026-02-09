/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

// Config defines the configuration for a meta-agent instance.
//   - Resp is the structured response type returned by the agent.
//   - CB is the type providing all tool callbacks.
type Config[Resp, CB any] struct {
	// SystemInstructions is the system prompt that defines the agent's role and behavior.
	SystemInstructions *promptbuilder.Prompt

	// UserPrompt is the template for formatting the user's request.
	// The Req type is bound to this template via its Bind method.
	UserPrompt *promptbuilder.Prompt

	// Tools provides all tool definitions for this agent.
	// Compose providers using toolcall.NewFindingToolsProvider,
	// toolcall.NewWorktreeToolsProvider, and toolcall.NewEmptyToolsProvider.
	Tools toolcall.ToolProvider[Resp, CB]
}
