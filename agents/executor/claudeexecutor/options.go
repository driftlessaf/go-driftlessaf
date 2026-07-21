/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// Option is a functional option for configuring the executor
type Option[Request promptbuilder.Bindable, Response any] func(*executor[Request, Response]) error

// WithMaxTokens sets the maximum tokens for responses
func WithMaxTokens[Request promptbuilder.Bindable, Response any](tokens int64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if tokens <= 0 {
			return fmt.Errorf("max tokens must be positive, got %d", tokens)
		}
		// 128000 is the max output-token ceiling for current Claude models (Sonnet 5,
		// Opus 4.6/4.7/4.8, Fable 5) on both the Vertex and Anthropic-direct backends. The
		// executor streams every response (Messages.NewStreaming), so values well above the
		// old 32000 cap do not risk the SDK's non-streaming HTTP timeout.
		if tokens > 128000 {
			return fmt.Errorf("max tokens %d exceeds maximum of 128000", tokens)
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

// WithEffort sets the reasoning effort (output_config.effort), which controls
// how deeply the model thinks and its overall token spend. The level is
// validated against the provider-neutral scale here and resolved against the
// configured model's supported set at request time: Opus 4.7+/Sonnet 5/
// Fable 5 take the full scale unchanged; models that predate "xhigh"
// (Sonnet 4.6, Opus 4.5/4.6) clamp it down to "high"; models without effort
// support drop the parameter — each with a logged warning, mirroring the
// nearest-supported mappings on the Gemini and OpenAI backends so a model
// swap never turns a tuned effort into a request error. Leaving it unset
// keeps the model default ("high" where supported). Effort is GA on every
// serving backend (Vertex AI and the first-party API) and needs no beta
// header; effort.XHigh is the recommended setting for hard coding/agentic
// work on the models that take it.
func WithEffort[Request promptbuilder.Bindable, Response any](level effort.Level) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if err := level.Validate(); err != nil {
			return err
		}
		e.effort = anthropic.OutputConfigEffort(level)
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

// WithToolCallConcurrency bounds how many of a single turn's tool calls run
// concurrently when the model emits more than one in a turn (parallel tool
// use). Defaults to DefaultToolCallConcurrency.
//
// Results are always consumed in the order the model emitted them, so the
// tool_use/tool_result pairing the API requires is preserved, and the first
// terminal result (in order) ends the run.
//
// A value of 1 runs the turn's tool calls strictly in order, one at a time.
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
type SubmitResultProvider[Response any] func() (claudetool.SubmitMetadata[Response], error)

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

// WithUserPromptSuffix renders a static, operator-authored prompt as a second
// text block of the initial user message, after the rendered request prompt.
//
// This exists for fleets of executions that share one large payload but vary
// a small trailing instruction — for example multi-pass reviewers that each
// examine the same changeset through a different lens. Anthropic prompt
// caching writes and reads cache entries at cache_control block boundaries,
// and a prefix is only shareable when the bytes up to a marked block are
// identical; splitting the initial message keeps the varying suffix out of
// the shared prefix, so the tool definitions, system prompt, and leading
// payload block are served from one cache entry across all such executions
// while the suffix block varies freely after the breakpoint.
//
// Setting this option implies WithCacheFirstUserBlock: the leading block gets
// the cache breakpoint that ends the shareable prefix — without that marker
// the split would buy nothing. The suffix must be fully bound by the caller;
// the request is never bound into it, and it is built once per Execute.
// See: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
func WithUserPromptSuffix[Request promptbuilder.Bindable, Response any](suffix *promptbuilder.Prompt) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if suffix == nil {
			return errors.New("user prompt suffix cannot be nil")
		}
		e.userPromptSuffix = suffix
		// The point of the split is a shareable leading block: place the
		// breakpoint that ends the shared prefix on it.
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

// WithForceSubmitToolChoice forces the model to call its terminal submit tool
// via tool_choice instead of leaving the choice to the model. This eliminates
// the wasted turn the executor would otherwise spend reactively redirecting a
// model that answered with plain text instead of calling the submit tool.
//
// The force is applied on the first turn when deferUntilToolName is empty or
// names a tool that is NOT registered for the run. When deferUntilToolName
// names a tool that IS registered (for example a deferred-evidence fetch tool),
// the first turn stays at tool_choice auto so the model can gather that
// deferred evidence first; the submit tool is forced only on the turn after
// that gate tool has been called at least once.
//
// The option is a no-op unless a terminal submit tool is configured via
// WithSubmitResultProvider — without one there is no tool to force toward. It
// is opt-in and off by default, so callers that do not set it keep the existing
// reactive behavior unchanged.
//
// Not compatible with WithThinking: the API requires tool_choice auto/none
// while extended thinking is active and returns a 400 for a forced tool_choice,
// so construction fails when both are set. The order in which the two options
// are applied does not matter — the conflict is checked after all options are
// applied.
func WithForceSubmitToolChoice[Request promptbuilder.Bindable, Response any](deferUntilToolName string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.forceSubmitToolChoice = true
		e.forceSubmitDeferUntilTool = deferUntilToolName
		return nil
	}
}

// Provider identifies the serving backend a Claude request goes to. The same
// Claude model can be served by two different providers with two different
// bills: Google Vertex AI (GCP billing) or the Anthropic first-party API via
// Workload Identity Federation (Anthropic workspace billing). The provider is
// stamped on every metric (gen_ai.provider.name) and trace turn (system), so
// stored telemetry distinguishes the backends explicitly instead of inferring
// them from model-ID shape (the Vertex-only "@version" suffix).
type Provider string

const (
	// ProviderVertex is Claude served by Google Vertex AI.
	ProviderVertex Provider = "vertex"
	// ProviderAnthropic is Claude served by the Anthropic first-party API.
	ProviderAnthropic Provider = "anthropic"
)

// metricName is the OTel gen_ai.provider.name value for the backend, aligned
// with the semconv well-known values the sibling executors use
// (googleexecutor: "gcp.vertex_ai", openaiexecutor: "openai-compat").
func (p Provider) metricName() string {
	if p == ProviderAnthropic {
		return "anthropic"
	}
	return "gcp.vertex_ai"
}

// traceSystem is the agenttrace system value for the backend.
func (p Provider) traceSystem() string {
	if p == ProviderAnthropic {
		return agenttrace.SystemAnthropic
	}
	return agenttrace.SystemGoogleVertex
}

// WithProvider declares which backend serves this executor's requests, so
// metrics and traces carry the true serving provider. Defaults to
// ProviderVertex, which matches anthropicauth.NewClient's fallback when no
// federation config is present; callers that construct a federation client
// must pass ProviderAnthropic.
func WithProvider[Request promptbuilder.Bindable, Response any](p Provider) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		switch p {
		case ProviderVertex, ProviderAnthropic:
			e.provider = p
			return nil
		default:
			return fmt.Errorf("unknown provider %q", p)
		}
	}
}

// WithResourceLabels sets labels for GCP billing attribution when using Claude via Vertex AI.
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
