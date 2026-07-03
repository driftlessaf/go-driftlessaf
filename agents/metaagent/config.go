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

	// MaxTurns sets the maximum number of conversation turns (LLM round-trips)
	// before the executor aborts. Zero means use the executor's default.
	MaxTurns int

	// ToolCallConcurrency bounds how many of a single turn's tool calls run
	// concurrently when the model emits more than one (parallel tool use).
	// Zero means use the executor's default (DefaultToolCallConcurrency). Set
	// to 1 to force strictly sequential tool dispatch — required for agents
	// whose tool handlers mutate shared state without their own synchronization.
	ToolCallConcurrency int

	// ThinkingBudget enables Claude extended thinking with the given token
	// budget when running on the Claude backend. Zero (the default) leaves
	// thinking disabled. Must be at least 1024 and less than the executor's
	// max tokens (32000 for meta-agents); see claudeexecutor.WithThinking.
	// On models where the Anthropic API has removed the explicit budget
	// parameter (Opus 4.7 and later), the executor automatically maps this to
	// adaptive thinking and the budget value is advisory only. No effect on
	// the Gemini backend.
	ThinkingBudget int64
}
