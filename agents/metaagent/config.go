/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"chainguard.dev/driftlessaf/agents/effort"
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

	// MaxTokens caps the model's output tokens per turn (the Anthropic
	// max_tokens parameter). Zero (the default) uses the meta-agent default of
	// 32000; the executor rejects values above 128000, the ceiling for current
	// Claude models. Because the executor streams every response, large values
	// do not risk the SDK's non-streaming HTTP timeout. Raise it for stages
	// whose turns need room for BOTH extended thinking and a tool call: at high
	// effort on a large context, adaptive thinking can otherwise consume the
	// whole budget and stop at max_tokens before the model emits its tool call,
	// which the executor surfaces as "no content in Claude's response". Claude
	// backend only; no effect on the Gemini or OpenAI backends.
	MaxTokens int64

	// ThinkingBudget enables Claude extended thinking with the given token
	// budget when running on the Claude backend. Zero (the default) leaves
	// thinking disabled. Must be at least 1024 and less than the executor's
	// max tokens (MaxTokens, or the 32000 default); see claudeexecutor.WithThinking.
	// On models where the Anthropic API has removed the explicit budget
	// parameter (Opus 4.7 and later), the executor automatically maps this to
	// adaptive thinking and the budget value is advisory only. No effect on
	// the Gemini or OpenAI backends.
	//
	// Deprecated: ThinkingBudget is Claude-only and already advisory-only on
	// Opus 4.7+. Use Effort, which works on every backend; do not set both.
	// The field will be removed once remaining consumers migrate.
	ThinkingBudget int64

	// Effort sets the provider-neutral reasoning-effort level, controlling how
	// deeply the model thinks and its overall token spend. Empty (the default)
	// leaves each backend's model default in place. Every backend maps the
	// level onto the nearest control the configured model supports:
	//   - Claude: output_config.effort — exact on Opus 4.7+/Sonnet 5/Fable 5;
	//     "xhigh" clamps to "high" on models that predate it (Sonnet 4.6,
	//     Opus 4.5/4.6); dropped with a warning on models without effort
	//     support; see claudeexecutor.WithEffort.
	//   - Gemini: thinkingLevel on Gemini 3.x models, thinkingBudget tiers on
	//     earlier models; see googleexecutor.WithEffort.
	//   - OpenAI-compatible: reasoning_effort, where xhigh and max clamp to
	//     "high"; reasoning models only, see openaiexecutor.WithEffort.
	// effort.XHigh is recommended for hard coding/agentic work on
	// Sonnet 5 / Opus 4.7+.
	Effort effort.Level

	// ResultValidators gate the terminal submit_result tool. When the model
	// submits a result that parses into Resp, every validator runs
	// concurrently against it; any findings reject the submission back to the
	// model as the tool's result — the agent loop continues until a submission
	// passes — and a validator error aborts the run. Empty (the default)
	// accepts every parsed submission. Findings are concatenated in
	// registration order. Validators must be safe for concurrent use; see
	// callbacks.ResultValidator.
	ResultValidators []callbacks.ResultValidator[Resp]

	// SuspendToolName, when non-empty, enables the ask-a-friend suspend/resume
	// capability: the backend advertises a held-out tool by this name, and when
	// the model calls it, Execute returns a *checkpoint.Suspension (extract it
	// with checkpoint.AsSuspension) carrying the envelope needed to resume the
	// paused conversation later, instead of a Resp. A resume-capable caller
	// obtains the resume path by type-asserting the constructed agent with
	// AsResumer.
	//
	// Claude backend only for now: the Gemini and OpenAI-compatible backends
	// reject a non-empty SuspendToolName at construction with a clear error
	// until their executors grow suspend support (DEV-2247 follow-up slices) —
	// silently ignoring it would advertise a lifecycle that can never fire.
	// The name must differ from the terminal submit tool's name and from every
	// caller-registered tool (both validated by the executor). Empty (the
	// default) leaves suspension disabled and the run's behavior byte-for-byte
	// unchanged.
	SuspendToolName string

	// SuspendToolDescription is the friend-facing description advertised to the
	// model for the suspend tool. Ignored when SuspendToolName is empty.
	SuspendToolDescription string
}

// suspendQuestionProperty is the single input property the suspend tool schema
// declares: the friend-facing question text. checkpoint.QuestionFromPending
// reads the same key ("question") when deriving a Question from a pending
// suspend call, so the schema and the extraction can never drift. Backends
// that gain suspend support later must build their schemas from this same
// constant.
const suspendQuestionProperty = "question"
