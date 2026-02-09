/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// GenAI provides OpenTelemetry metrics for generative AI operations.
// It includes counters for token usage (prompt and completion) and tool calls,
// with support for graceful degradation if metric creation fails.
type GenAI struct {
	meter            metric.Meter
	promptTokens     metric.Int64Counter
	completionTokens metric.Int64Counter
	toolCallCounter  metric.Int64Counter
	attrEnricher     AttributeEnricher
}

// NewGenAI creates a new GenAI metrics instance with the specified meter name.
// Uses graceful degradation: if any metric counter fails to initialize, logs a warning
// and uses a no-op counter instead of failing entirely.
//
// The meterName should be unified across all agent executors (e.g., "chainguard.ai.agents")
// with the model name serving as a dimension on the recorded metrics to differentiate
// between different models (Claude, Gemini, etc.).
func NewGenAI(meterName string) *GenAI {
	meter := otel.Meter(meterName, metric.WithInstrumentationVersion("1.0.0"))

	// Create prompt tokens counter with graceful degradation
	promptTokens, err := meter.Int64Counter("genai.token.prompt",
		metric.WithDescription("The number of prompt tokens used"),
		metric.WithUnit("{tokens}"))
	if err != nil {
		slog.Warn("Failed to create prompt tokens counter, metrics will be disabled", "error", err, "meter", meterName)
		promptTokens = noop.Int64Counter{}
	}

	// Create completion tokens counter with graceful degradation
	completionTokens, err := meter.Int64Counter("genai.token.completion",
		metric.WithDescription("The number of completion tokens used"),
		metric.WithUnit("{tokens}"))
	if err != nil {
		slog.Warn("Failed to create completion tokens counter, metrics will be disabled", "error", err, "meter", meterName)
		completionTokens = noop.Int64Counter{}
	}

	// Create tool call counter with graceful degradation
	toolCallCounter, err := meter.Int64Counter("genai.tool.calls",
		metric.WithDescription("The number of tool calls made during execution"),
		metric.WithUnit("{calls}"))
	if err != nil {
		slog.Warn("Failed to create tool call counter, metrics will be disabled", "error", err, "meter", meterName)
		toolCallCounter = noop.Int64Counter{}
	}

	return &GenAI{
		meter:            meter,
		promptTokens:     promptTokens,
		completionTokens: completionTokens,
		toolCallCounter:  toolCallCounter,
	}
}

// SetAttributeEnricher sets the attribute enricher for this metrics instance.
// The enricher is called before recording each metric to add contextual attributes
// (e.g., repository, pull_request, commit_sha, turn).
func (m *GenAI) SetAttributeEnricher(enricher AttributeEnricher) {
	m.attrEnricher = enricher
}

// RecordTokens records prompt and completion token usage with optional enrichment.
// The model parameter is added as a base attribute, and the enricher (if set)
// can add additional contextual attributes.
func (m *GenAI) RecordTokens(ctx context.Context, model string, promptTokens, completionTokens int64, attrs ...attribute.KeyValue) {
	// Base attributes
	baseAttrs := []attribute.KeyValue{
		attribute.String("model", model),
	}

	// Enrich with application-specific attributes
	if m.attrEnricher != nil {
		baseAttrs = m.attrEnricher(ctx, baseAttrs)
	}

	baseAttrs = append(baseAttrs, attrs...)

	// Record token metrics
	m.promptTokens.Add(ctx, promptTokens, metric.WithAttributes(baseAttrs...))
	m.completionTokens.Add(ctx, completionTokens, metric.WithAttributes(baseAttrs...))
}

// RecordToolCall records a tool invocation with optional enrichment.
// The model and toolName parameters are added as base attributes, and the enricher
// (if set) can add additional contextual attributes.
func (m *GenAI) RecordToolCall(ctx context.Context, model, toolName string, attrs ...attribute.KeyValue) {
	// Base attributes
	baseAttrs := []attribute.KeyValue{
		attribute.String("model", model),
		attribute.String("tool", toolName),
	}

	// Enrich with application-specific attributes
	if m.attrEnricher != nil {
		baseAttrs = m.attrEnricher(ctx, baseAttrs)
	}

	baseAttrs = append(baseAttrs, attrs...)

	// Record tool call
	m.toolCallCounter.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
}
