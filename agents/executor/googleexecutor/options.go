/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// Option is a functional option for configuring an executor
type Option[Request promptbuilder.Bindable, Response any] func(*executor[Request, Response]) error

// WithModel sets the model to use for generation
func WithModel[Request promptbuilder.Bindable, Response any](model string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if !strings.HasPrefix(model, "gemini-") {
			return fmt.Errorf("model %q does not appear to be a Gemini model (expected gemini-* format)", model)
		}
		e.model = model
		return nil
	}
}

// WithTemperature sets the temperature for generation
// Gemini models support temperature values from 0.0 to 2.0
// This is a wider range than Claude (0.0-1.0) allowing for more creative outputs
// Lower values (e.g., 0.1) produce more deterministic outputs
// Higher values (e.g., 1.5-2.0) produce very creative/random outputs
func WithTemperature[Request promptbuilder.Bindable, Response any](temperature float32) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if temperature < 0.0 || temperature > 2.0 {
			return fmt.Errorf("temperature must be between 0.0 and 2.0, got %f", temperature)
		}
		e.temperature = temperature
		return nil
	}
}

// WithMaxOutputTokens sets the maximum output tokens for generation
func WithMaxOutputTokens[Request promptbuilder.Bindable, Response any](tokens int32) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if tokens <= 0 {
			return fmt.Errorf("max output tokens must be positive, got %d", tokens)
		}
		// Gemini models support up to 8192 tokens by default, some support more
		// Gemini 3.1 Pro supports up to 65536 output tokens
		if tokens > 65536 {
			return fmt.Errorf("max output tokens %d exceeds maximum of 65536", tokens)
		}
		e.maxOutputTokens = tokens
		return nil
	}
}

// WithMaxTurns sets the maximum number of conversation turns (LLM round-trips)
// before the executor aborts. This prevents runaway loops where the model
// keeps calling tools without converging on a result.
// Default is DefaultMaxTurns.
func WithMaxTurns[Request promptbuilder.Bindable, Response any](turns int) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if turns <= 0 {
			return fmt.Errorf("max turns must be positive, got %d", turns)
		}
		e.maxTurns = turns
		return nil
	}
}

// WithToolCallConcurrency bounds how many of a single turn's function calls run
// concurrently when the model emits more than one in a turn (parallel function
// calling). Defaults to DefaultToolCallConcurrency.
//
// Response parts are always consumed in the order the model emitted the calls,
// and the first terminal result (in order) ends the run.
//
// A value of 1 runs the turn's function calls strictly in order, one at a time.
// Set it to 1 for agents whose tool handlers mutate shared state (a worktree, a
// cache) without their own synchronization; concurrent dispatch is otherwise
// safe because handlers share only the trace, which is concurrency-safe.
func WithToolCallConcurrency[Request promptbuilder.Bindable, Response any](n int) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if n < 1 {
			return fmt.Errorf("tool call concurrency must be at least 1, got %d", n)
		}
		e.toolCallConcurrency = n
		return nil
	}
}

// WithSystemInstructions sets the system instructions for the model
func WithSystemInstructions[Request promptbuilder.Bindable, Response any](prompt *promptbuilder.Prompt) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if prompt == nil {
			return errors.New("system instructions prompt cannot be nil")
		}
		e.systemInstructions = prompt
		return nil
	}
}

// WithUserPromptSuffix appends a static, operator-authored prompt to the end
// of the built user prompt, separated by a blank line. It is the Gemini
// counterpart of the Claude executor's user-prompt-suffix option: agents that
// share one large payload but vary a small trailing instruction (for example
// multi-pass reviewers examining one changeset through different lenses) keep
// the payload in the main prompt and the varying instruction in the suffix.
// Vertex AI context caching has no per-block prefix semantics — it caches
// system instructions and tools via CachedContent — so the suffix is simply
// concatenated and there is no cache-shaping side effect. The suffix must be
// fully bound by the caller; the request is never bound into it.
func WithUserPromptSuffix[Request promptbuilder.Bindable, Response any](suffix *promptbuilder.Prompt) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if suffix == nil {
			return errors.New("user prompt suffix cannot be nil")
		}
		e.userPromptSuffix = suffix
		return nil
	}
}

// WithResponseMIMEType sets the response MIME type (e.g., "application/json")
func WithResponseMIMEType[Request promptbuilder.Bindable, Response any](mimeType string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if mimeType != "" && mimeType != "application/json" && mimeType != "text/plain" {
			return fmt.Errorf("unsupported MIME type %q, must be 'application/json' or 'text/plain'", mimeType)
		}
		e.responseMIMEType = mimeType
		return nil
	}
}

// WithResponseSchema sets the response schema for structured output
func WithResponseSchema[Request promptbuilder.Bindable, Response any](schema *genai.Schema) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.responseSchema = schema
		return nil
	}
}

// WithThinking enables thinking mode with the specified token budget
// The budget parameter sets the maximum tokens the model can use for reasoning
// Special value -1 enables dynamic thinking where the model adjusts based on complexity
// See https://ai.google.dev/gemini-api/docs/thinking
// Must be less than max_output_tokens to leave room for actual output
func WithThinking[Request promptbuilder.Bindable, Response any](budgetTokens int32) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		// Gemini models allow -1 for dynamic thinking
		// See https://ai.google.dev/gemini-api/docs/thinking#set-budget
		if budgetTokens == -1 {
			e.thinkingBudget = &budgetTokens
			return nil
		}
		if budgetTokens <= 0 {
			return fmt.Errorf("thinking budget must be positive (or -1 for dynamic), got %d", budgetTokens)
		}

		// Must be less than maxOutputTokens because the API counts
		// thoughts_token_count + output_token_count together against the limit
		if budgetTokens >= e.maxOutputTokens {
			return fmt.Errorf("thinking budget (%d) must be less than max_output_tokens (%d)", budgetTokens, e.maxOutputTokens)
		}
		e.thinkingBudget = &budgetTokens
		return nil
	}
}

// SubmitResultProvider constructs tool metadata for submit_result.
type SubmitResultProvider[Response any] func() (googletool.SubmitMetadata[Response], error)

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

// WithResultValidator registers a validator that gates the terminal submit
// tool. When the model calls the submit tool with a payload that parses, every
// registered validator runs concurrently against the parsed response; any
// findings reject the submission back to the model as the tool's result — the
// loop continues until a submission passes — and a validator error aborts the
// run. Repeatable: each call appends a validator, and their findings are
// concatenated in registration order. Only meaningful when a submit tool is
// configured via WithSubmitResultProvider; without one there is nothing to
// gate.
func WithResultValidator[Request promptbuilder.Bindable, Response any](v callbacks.ResultValidator[Response]) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if v == nil {
			return errors.New("result validator cannot be nil")
		}
		e.resultValidators = append(e.resultValidators, v)
		return nil
	}
}

// WithRetryConfig sets the retry configuration for handling transient Vertex AI errors.
// This is particularly useful for handling 429 RESOURCE_EXHAUSTED errors that occur
// when quota limits are hit. If not set, a default configuration is used.
func WithRetryConfig[Request promptbuilder.Bindable, Response any](cfg retry.RetryConfig) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if err := cfg.Validate(); err != nil {
			return err
		}
		e.retryConfig = cfg
		return nil
	}
}

// WithoutCacheControl disables Vertex AI context caching.
//
// Context caching is enabled by default because it significantly reduces input
// token costs for multi-turn agentic workflows. The API caches the system
// instructions and tool definitions in a CachedContent resource, serving cached
// tokens at reduced cost. The cache has a configurable TTL (default 30 minutes).
//
// You would only disable this if you have a single-turn agent with a very short
// tool/system prompt that doesn't benefit from caching, or for debugging.
// See: https://cloud.google.com/vertex-ai/generative-ai/docs/context-cache/context-cache-overview
func WithoutCacheControl[Request promptbuilder.Bindable, Response any]() Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.cacheControl = false
		return nil
	}
}

// WithCacheTTL sets the TTL for Vertex AI cached content resources.
// Default is 30 minutes. Minimum is 1 minute.
// For long-running agents that make many turns, consider a longer TTL.
func WithCacheTTL[Request promptbuilder.Bindable, Response any](ttl time.Duration) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if ttl < time.Minute {
			return fmt.Errorf("cache TTL must be at least 1 minute, got %v", ttl)
		}
		e.cacheTTL = ttl
		return nil
	}
}

// WithResourceLabels sets labels that are sent with each Vertex AI API request.
// Automatically includes default labels from environment variables:
//   - service_name: from K_SERVICE, falling back to CLOUD_RUN_JOB (defaults to "unknown")
//   - product: from CHAINGUARD_PRODUCT (defaults to "unknown")
//   - team: from CHAINGUARD_TEAM (defaults to "unknown")
//
// Custom labels passed to this function will override defaults if they use the same keys.
func WithResourceLabels[Request promptbuilder.Bindable, Response any](labels map[string]string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.resourceLabels = execshared.DefaultResourceLabels(labels)
		return nil
	}
}
