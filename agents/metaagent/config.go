/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
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

	// UserPromptSuffix is an optional static prompt appended as a separate
	// trailing block of the initial user message, after the bound UserPrompt.
	// When set, the leading block (UserPrompt with the request bound) gets a
	// cache breakpoint so executions that differ only in the suffix share its
	// cache entry. Intended for multi-pass agents reviewing one payload
	// through different lenses: the payload rides in UserPrompt, the per-pass
	// lens in the suffix. The request is never bound into the suffix — it
	// must be fully bound already. On the Gemini and OpenAI-compatible
	// backends (no per-block cache semantics) the built suffix is
	// concatenated onto the user prompt with a blank-line separator.
	UserPromptSuffix *promptbuilder.Prompt

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

	// Effort sets the Claude reasoning effort (output_config.effort): one of
	// "low", "medium", "high", "xhigh", "max". Empty (the default) leaves the
	// model default ("high"). "xhigh" is recommended for hard coding/agentic
	// work on Sonnet 5 / Opus 4.7+. Claude backend only; no effect on the
	// Gemini or OpenAI backends. See claudeexecutor.WithEffort.
	Effort string

	// ResultValidators gate the terminal submit_result tool. When the model
	// submits a result that parses into Resp, every validator runs
	// concurrently against it; any findings reject the submission back to the
	// model as the tool's result — the agent loop continues until a submission
	// passes — and a validator error aborts the run. Empty (the default)
	// accepts every parsed submission. Findings are concatenated in
	// registration order. Validators must be safe for concurrent use; see
	// callbacks.ResultValidator.
	ResultValidators []callbacks.ResultValidator[Resp]
}
