/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// Option is a functional option for configuring the executor
type Option[Request promptbuilder.Bindable, Response any] func(*executor[Request, Response]) error

// WithMaxTokens sets the maximum tokens for responses
func WithMaxTokens[Request promptbuilder.Bindable, Response any](tokens int64) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if tokens <= 0 {
			return fmt.Errorf("max tokens must be positive, got %d", tokens)
		}
		if tokens > 32000 { // Maximum for Opus
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
type SubmitResultProvider[Response any] func() (ToolMetadata[Response], error)

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

// WithAttributeEnricher sets a custom attribute enricher for metrics.
// The enricher is called before recording each metric, allowing the application
// to add contextual attributes (e.g., repository, pull_request, package_version, etc.)
// If not provided, metrics will only include base attributes (model, tool).
func WithAttributeEnricher[Request promptbuilder.Bindable, Response any](enricher metrics.AttributeEnricher) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.genaiMetrics.SetAttributeEnricher(enricher)
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
