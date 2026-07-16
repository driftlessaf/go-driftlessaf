/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/executor/internal/execshared"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
)

// Option is a functional option for configuring the executor.
type Option[Request promptbuilder.Bindable, Response any] func(*executor[Request, Response]) error

// WithModel sets the model name.
func WithModel[Request promptbuilder.Bindable, Response any](model string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if model == "" {
			return errors.New("model name cannot be empty")
		}
		e.modelName = model
		return nil
	}
}

// WithTemperature sets the sampling temperature (0.0–2.0).
func WithTemperature[Request promptbuilder.Bindable, Response any](temp float64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if temp < 0.0 || temp > 2.0 {
			return fmt.Errorf("temperature must be between 0.0 and 2.0, got %f", temp)
		}
		e.temperature = temp
		return nil
	}
}

// WithMaxTokens sets the maximum completion tokens.
func WithMaxTokens[Request promptbuilder.Bindable, Response any](tokens int64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if tokens <= 0 {
			return fmt.Errorf("max tokens must be positive, got %d", tokens)
		}
		e.maxTokens = tokens
		return nil
	}
}

// WithMaxTurns sets the maximum number of conversation turns before aborting.
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
// calls). Defaults to DefaultToolCallConcurrency.
//
// Tool result messages are always appended in the order the model emitted the
// calls. A value of 1 forces strictly sequential dispatch. Set it to 1 for
// agents whose tool handlers mutate shared state (a worktree, a cache) without
// their own synchronization; concurrent dispatch is otherwise safe because
// handlers share only the trace, which is concurrency-safe.
//
// Note: some OpenAI-compatible models disable parallel tool calls when strict
// structured output is in force, in which case the model emits one tool call
// per turn and this option has no effect.
func WithToolCallConcurrency[Request promptbuilder.Bindable, Response any](n int) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if n < 1 {
			return fmt.Errorf("tool call concurrency must be at least 1, got %d", n)
		}
		e.toolCallConcurrency = n
		return nil
	}
}

// WithSystemInstructions sets the system prompt.
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
// of the built user prompt, separated by a blank line. It is the
// OpenAI-compatible counterpart of the Claude executor's user-prompt-suffix
// option: agents that share one large payload but vary a small trailing
// instruction (for example multi-pass reviewers examining one changeset
// through different lenses) keep the payload in the main prompt and the
// varying instruction in the suffix. The OpenAI-compatible API has no
// per-block prompt-cache semantics, so the suffix is simply concatenated and
// there is no cache-shaping side effect. The suffix must be fully bound by
// the caller; the request is never bound into it.
func WithUserPromptSuffix[Request promptbuilder.Bindable, Response any](suffix *promptbuilder.Prompt) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if suffix == nil {
			return errors.New("user prompt suffix cannot be nil")
		}
		e.userPromptSuffix = suffix
		return nil
	}
}

// SubmitResultProvider constructs tool metadata for submit_result.
type SubmitResultProvider[Response any] func() (openaistool.SubmitMetadata[Response], error)

// WithSubmitResultProvider registers the submit_result tool.
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

// WithRetryConfig sets the retry configuration for transient API errors.
func WithRetryConfig[Request promptbuilder.Bindable, Response any](cfg retry.RetryConfig) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if err := cfg.Validate(); err != nil {
			return err
		}
		e.retryConfig = cfg
		return nil
	}
}

// WithResourceLabels sets labels for observability attribution.
// Automatically includes default labels from environment variables:
//   - service_name: from K_SERVICE, falling back to CLOUD_RUN_JOB (defaults to "unknown")
//   - product: from CHAINGUARD_PRODUCT (defaults to "unknown")
//   - team: from CHAINGUARD_TEAM (defaults to "unknown")
func WithResourceLabels[Request promptbuilder.Bindable, Response any](labels map[string]string) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.resourceLabels = execshared.DefaultResourceLabels(labels)
		return nil
	}
}
