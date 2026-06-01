/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

// Option is a functional option for configuring the executor
type Option[Request promptbuilder.Bindable, Response any] func(*executor[Request, Response]) error

// WithMaxTokens sets the maximum tokens for responses
func WithMaxTokens[Request promptbuilder.Bindable, Response any](tokens int64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if tokens <= 0 {
			return fmt.Errorf("max tokens must be positive, got %d", tokens)
		}
		if tokens > 32000 { // Maximum output tokens for Claude models on Vertex AI
			return fmt.Errorf("max tokens %d exceeds maximum of 32000", tokens)
		}
		e.maxTokens = tokens
		return nil
	}
}

// WithTemperature sets the temperature for responses
// Claude models support temperature values from 0.0 to 1.0
// Lower values (e.g., 0.1) produce more deterministic outputs
// Higher values (e.g., 0.9) produce more creative/random outputs
func WithTemperature[Request promptbuilder.Bindable, Response any](temp float64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if temp < 0.0 || temp > 1.0 {
			return fmt.Errorf("temperature must be between 0.0 and 1.0, got %f", temp)
		}
		e.temperature = temp
		e.temperatureSet = true
		return nil
	}
}

// WithMaxTurns sets the maximum number of conversation turns (LLM round-trips)
// before the executor aborts. This prevents runaway loops where the model
// keeps calling tools without converging on a result.
// Default is DefaultMaxTurns (50).
func WithMaxTurns[Request promptbuilder.Bindable, Response any](turns int) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if turns <= 0 {
			return fmt.Errorf("max turns must be positive, got %d", turns)
		}
		e.maxTurns = turns
		return nil
	}
}

// WithSystemInstructions sets custom system instructions
func WithSystemInstructions[Request promptbuilder.Bindable, Response any](prompt *promptbuilder.Prompt) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if prompt == nil {
			return errors.New("system instructions prompt cannot be nil")
		}
		e.systemInstructions = prompt
		return nil
	}
}

// WithModel allows overriding the model name
func WithModel[Request promptbuilder.Bindable, Response any](model string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if !strings.HasPrefix(model, "claude-") {
			return fmt.Errorf("model %q does not appear to be a Claude model (expected claude-* format)", model)
		}
		e.modelName = model
		return nil
	}
}

// WithThinking enables extended thinking mode with the specified token budget
// The budget_tokens parameter sets the maximum tokens Claude can use for reasoning
// This must be less than max_tokens and at least 1024 tokens is recommended
func WithThinking[Request promptbuilder.Bindable, Response any](budgetTokens int64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if budgetTokens < 1024 {
			return fmt.Errorf("thinking budget_tokens must be at least 1024, got %d", budgetTokens)
		}
		if budgetTokens >= e.maxTokens {
			return fmt.Errorf("thinking budget_tokens (%d) must be less than max_tokens (%d)", budgetTokens, e.maxTokens)
		}
		e.thinkingBudgetTokens = &budgetTokens
		return nil
	}
}

// SubmitResultProvider constructs tool metadata for submit_result.
type SubmitResultProvider[Response any] func() (claudetool.Metadata[Response], error)

// WithSubmitResultProvider registers the submit_result tool using the supplied provider.
// This is opt-in - agents must explicitly call this to enable submit_result.
func WithSubmitResultProvider[Request promptbuilder.Bindable, Response any](provider SubmitResultProvider[Response]) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if provider == nil {
			return errors.New("submit_result provider cannot be nil")
		}
		tool, err := provider()
		if err != nil {
			return err
		}
		e.submitTool = tool
		return nil
	}
}

// WithRetryConfig sets the retry configuration for handling transient Claude API errors.
// This is particularly useful for handling 429 rate limit and 529 overloaded errors.
// If not set, a default configuration is used.
func WithRetryConfig[Request promptbuilder.Bindable, Response any](cfg retry.RetryConfig) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if err := cfg.Validate(); err != nil {
			return err
		}
		e.retryConfig = cfg
		return nil
	}
}

// WithoutCacheControl disables Anthropic prompt caching.
//
// Prompt caching is enabled by default because it significantly reduces input token
// costs for multi-turn agentic workflows. The API caches the request prefix (tool
// definitions + system prompt) and serves it at 10% of the normal input token price
// on subsequent turns. The only cost is a 1.25x write premium on the first turn,
// which is amortized across all subsequent cache reads within the 5-min TTL.
//
// You would only disable this if you have a single-turn agent that runs less than
// once every 5 minutes, where the 1.25x write cost would never be recouped.
// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
func WithoutCacheControl[Request promptbuilder.Bindable, Response any]() Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.cacheControl = false
		return nil
	}
}

// WithCacheFirstUserBlock places an additional Anthropic cache breakpoint on
// the first user content block (the rendered prompt), in addition to the tool
// definitions and system prompt that are cached by default.
//
// This is useful when the first user message carries a large payload (for
// example, per-request evidence embedded in the prompt) and the agent loop
// spans several turns: with the breakpoint, the API reads that payload from
// cache at 10% of the base input price on turns after the first, instead of
// re-billing it at full price each turn.
//
// The breakpoint is only placed when prompt caching is enabled and a breakpoint
// slot remains within the API's limit, so it can never cause the API to reject
// a request for having too many breakpoints. Off by default.
//
// Caching benefits accrue within a single model's iteration loop. A workflow
// that switches models mid-flight (for example, a cheap first pass that
// escalates to a stronger model) does not share a cached prefix across the
// switch, because the cache is keyed by the exact request prefix including the
// model.
// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
func WithCacheFirstUserBlock[Request promptbuilder.Bindable, Response any]() Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.cacheFirstUserBlock = true
		return nil
	}
}

// WithMaxToolCallsBeforeFinalize bounds the agentic loop with a soft cap: once
// the model has made n investigative (non-terminal) tool calls, the executor
// injects a single instruction asking it to call its terminal submit tool now
// and forces that tool on the next turn.
//
// This is distinct from WithMaxTurns, which aborts the run when exceeded. The
// soft cap instead steers the model toward emitting a result based on the
// evidence gathered so far, which is preferable for workflows that should
// always return a verdict rather than fail. A value of zero (the default)
// disables the nudge entirely. The nudge only takes effect when a terminal
// tool is configured via WithSubmitResultProvider; without one there is no
// tool to steer toward, so the option is a no-op.
//
// Not compatible with WithThinking; the API requires tool_choice auto/none
// while thinking is active, and the forced tool_choice this option uses on
// the finalize turn returns a 400.
func WithMaxToolCallsBeforeFinalize[Request promptbuilder.Bindable, Response any](n int) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if n < 0 {
			return fmt.Errorf("max tool calls before finalize must be non-negative, got %d", n)
		}
		e.maxToolCallsBeforeFinalize = n
		return nil
	}
}

// WithResourceLabels sets labels for GCP billing attribution when using Claude via Vertex AI.
// Automatically includes default labels from environment variables:
//   - service_name: from K_SERVICE (defaults to "unknown")
//   - product: from CHAINGUARD_PRODUCT (defaults to "unknown")
//   - team: from CHAINGUARD_TEAM (defaults to "unknown")
//
// Custom labels passed to this function will override defaults if they use the same keys.
func WithResourceLabels[Request promptbuilder.Bindable, Response any](labels map[string]string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		// Start with default labels from environment
		serviceName := os.Getenv("K_SERVICE")
		if serviceName == "" {
			serviceName = "unknown"
		}
		productName := os.Getenv("CHAINGUARD_PRODUCT")
		if productName == "" {
			productName = "unknown"
		}
		teamName := os.Getenv("CHAINGUARD_TEAM")
		if teamName == "" {
			teamName = "unknown"
		}

		e.resourceLabels = map[string]string{
			"service_name": serviceName,
			"product":      productName,
			"team":         teamName,
		}

		// Merge custom labels (these will override defaults if keys match)
		if labels != nil {
			maps.Copy(e.resourceLabels, labels)
		}
		return nil
	}
}
