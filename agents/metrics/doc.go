/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package metrics provides OpenTelemetry metrics instrumentation for generative AI operations.

This package simplifies the process of tracking token usage and tool calls across AI model
interactions. It provides a unified metrics interface that works with any AI model provider
(Claude, Gemini, OpenAI, etc.) using OpenTelemetry for observability.

# Overview

The metrics package offers the following key features:

  - Token usage tracking (prompt and completion tokens)
  - Tool call counting with model and tool name dimensions
  - Automatic attribute enrichment from execution context (repository, turn, etc.)
  - Graceful degradation when metric creation fails
  - Thread-safe operations

# Basic Usage

Create a GenAI metrics instance and record token usage:

	// Create metrics instance with a unified meter name
	m := metrics.NewGenAI("chainguard.ai.agents")

	// Record token usage for a model interaction
	m.RecordTokens(ctx, "claude-3-sonnet", 150, 250)

	// Record a tool call
	m.RecordToolCall(ctx, "claude-3-sonnet", "read_file")

# Attribute Enrichment

Contextual attributes (reconciler_type, repository, turn, etc.) are automatically
extracted from the execution context propagated via context.Context using
agenttrace.GetExecutionContext. No manual enricher setup is needed.

# Graceful Degradation

The package handles metric creation failures gracefully. If a counter cannot be created,
a warning is logged and a no-op counter is used instead:

	// Even if OpenTelemetry is not configured, this won't panic
	m := metrics.NewGenAI("chainguard.ai.agents")
	m.RecordTokens(ctx, "claude-3-sonnet", 100, 200) // Safe to call

# Thread Safety

All functions and methods in this package are thread-safe. The GenAI struct can be
safely shared across goroutines for concurrent metric recording.

# Integration with AI Executors

This package is designed to work with various AI executor implementations:

Claude executor example:

	m := metrics.NewGenAI("chainguard.ai.agents")

	// After each Claude API call
	m.RecordTokens(ctx, "claude-3-sonnet",
		response.Usage.InputTokens,
		response.Usage.OutputTokens)

	for _, toolUse := range response.Content {
		if toolUse.Type == "tool_use" {
			m.RecordToolCall(ctx, "claude-3-sonnet", toolUse.Name)
		}
	}

Google Gemini executor example:

	m := metrics.NewGenAI("chainguard.ai.agents")

	// After each Gemini API call
	m.RecordTokens(ctx, "gemini-pro",
		response.UsageMetadata.PromptTokenCount,
		response.UsageMetadata.CandidatesTokenCount)

# Metric Names

The package emits the following OpenTelemetry metrics:

Custom metrics:

  - genai.token.prompt: Counter of prompt tokens used (unit: {tokens})
  - genai.token.completion: Counter of completion tokens used (unit: {tokens})
  - genai.tool.calls: Counter of tool invocations (unit: {calls})

OpenTelemetry GenAI semantic convention metric:

  - gen_ai.client.token.usage: Histogram of token counts with gen_ai.token.type dimension (unit: {token})

All metrics include a "model" attribute. Tool call metrics also include a "tool" attribute.
The semconv histogram also includes gen_ai.operation.name and gen_ai.request.model attributes.
Callers should pass gen_ai.provider.name via the attrs parameter.

# Custom Attributes

Additional attributes can be passed to recording methods:

	m.RecordTokens(ctx, "claude-3-sonnet", 100, 200,
		attribute.String("agent", "code-reviewer"),
		attribute.Int("turn", 3))

	m.RecordToolCall(ctx, "claude-3-sonnet", "edit_file",
		attribute.String("file_type", "go"))
*/
package metrics
