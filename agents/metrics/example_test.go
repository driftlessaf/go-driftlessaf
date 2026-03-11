/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/metrics"
	"go.opentelemetry.io/otel/attribute"
)

// ExampleNewGenAI demonstrates creating a new GenAI metrics instance.
func ExampleNewGenAI() {
	// Create a metrics instance with a unified meter name
	// The meter name should be consistent across all agent executors
	m := metrics.NewGenAI("chainguard.ai.agents")

	// The metrics instance is ready to use
	fmt.Printf("Metrics instance created: %T\n", m)

	// Output:
	// Metrics instance created: *metrics.GenAI
}

// ExampleGenAI_RecordTokens demonstrates recording token usage.
func ExampleGenAI_RecordTokens() {
	ctx := context.Background()
	m := metrics.NewGenAI("chainguard.ai.agents")

	// Record token usage from an AI model response
	// Parameters: context, model name, prompt tokens, completion tokens
	m.RecordTokens(ctx, "claude-3-sonnet", 150, 250)

	// Record with additional custom attributes
	m.RecordTokens(ctx, "gemini-pro", 100, 200,
		attribute.String("agent", "code-reviewer"),
		attribute.Int("turn", 1))

	fmt.Println("Token metrics recorded")

	// Output:
	// Token metrics recorded
}

// ExampleGenAI_RecordToolCall demonstrates recording tool invocations.
func ExampleGenAI_RecordToolCall() {
	ctx := context.Background()
	m := metrics.NewGenAI("chainguard.ai.agents")

	// Record a tool call with model and tool name
	m.RecordToolCall(ctx, "claude-3-sonnet", "read_file")

	// Record with additional custom attributes
	m.RecordToolCall(ctx, "claude-3-sonnet", "edit_file",
		attribute.String("file_type", "go"),
		attribute.String("operation", "modify"))

	fmt.Println("Tool call metrics recorded")

	// Output:
	// Tool call metrics recorded
}

// ExampleGenAI_multipleModels demonstrates tracking metrics across different models.
func ExampleGenAI_multipleModels() {
	ctx := context.Background()

	// Use a single metrics instance for all models
	// The model name serves as a dimension to differentiate between them
	m := metrics.NewGenAI("chainguard.ai.agents")

	// Track Claude usage
	m.RecordTokens(ctx, "claude-3-sonnet", 150, 250)
	m.RecordToolCall(ctx, "claude-3-sonnet", "read_file")

	// Track Gemini usage
	m.RecordTokens(ctx, "gemini-pro", 100, 300)
	m.RecordToolCall(ctx, "gemini-pro", "search_codebase")

	// Track GPT usage
	m.RecordTokens(ctx, "gpt-4", 200, 400)

	fmt.Println("Multi-model metrics recorded")

	// Output:
	// Multi-model metrics recorded
}
