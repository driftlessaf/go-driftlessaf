/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"

	"chainguard.dev/driftlessaf/agents/executor/retry"
	"chainguard.dev/driftlessaf/agents/metrics"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
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
		if tokens > 32768 {
			return fmt.Errorf("max output tokens %d exceeds maximum of 32768", tokens)
		}
		e.maxOutputTokens = tokens
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
type SubmitResultProvider[Response any] func() (googletool.Metadata[Response], error)

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

// WithResourceLabels sets labels that are sent with each Vertex AI API request.
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
