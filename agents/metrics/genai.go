/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metrics

import (
	"context"
	"slices"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// GenAI provides OpenTelemetry metrics for generative AI operations.
// It includes counters for token usage (prompt and completion), tool calls,
// and prompt cache metrics, with support for graceful degradation if metric
// creation fails.
//
// Metrics are emitted in both custom format (genai.token.prompt, genai.token.completion)
// and OpenTelemetry GenAI semantic conventions (gen_ai.client.token.usage histogram
// with gen_ai.token.type dimension).
// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/
type GenAI struct {
	promptTokens     metric.Int64Counter
	completionTokens metric.Int64Counter
	toolCallCounter  metric.Int64Counter
	// OpenTelemetry GenAI semconv: token usage histogram with token type dimension.
	// Uses explicit bucket boundaries from the spec.
	semconvTokenUsage metric.Float64Histogram
	// agentTurns tracks the number of LLM round-trips per execution.
	// Recorded on every exit path so p50/p95/p99 are observable per service.
	agentTurns metric.Int64Histogram
	// turnLimitExceeded counts executions aborted by the maxTurns hard limit.
	turnLimitExceeded metric.Int64Counter
	// apiRequests counts every LLM API attempt with a response_code attribute,
	// so 200/429/529/5xx all fall out of the same series. Mirrors the shape of
	// serviceruntime.googleapis.com/api/request_count but on our side of the
	// wire — and crucially carries the resource-label attributes
	// (service_name, agent_name, model_name) that the GCP-side metric lacks.
	apiRequests metric.Int64Counter
}

// newInstrument creates a metric instrument with graceful degradation: if
// creation fails, it logs warnMsg and returns the no-op fallback so metrics
// are disabled instead of failing entirely.
func newInstrument[I any, O any](create func(string, ...O) (I, error), fallback I, name, warnMsg, meterName string, opts ...O) I {
	inst, err := create(name, opts...)
	if err != nil {
		clog.WarnContext(context.Background(), warnMsg, "error", err, "meter", meterName)
		return fallback
	}
	return inst
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

	return &GenAI{
		promptTokens: newInstrument[metric.Int64Counter, metric.Int64CounterOption](meter.Int64Counter, noop.Int64Counter{},
			"genai.token.prompt",
			"Failed to create prompt tokens counter, metrics will be disabled", meterName,
			metric.WithDescription("The number of prompt tokens used"),
			metric.WithUnit("{tokens}")),
		completionTokens: newInstrument[metric.Int64Counter, metric.Int64CounterOption](meter.Int64Counter, noop.Int64Counter{},
			"genai.token.completion",
			"Failed to create completion tokens counter, metrics will be disabled", meterName,
			metric.WithDescription("The number of completion tokens used"),
			metric.WithUnit("{tokens}")),
		toolCallCounter: newInstrument[metric.Int64Counter, metric.Int64CounterOption](meter.Int64Counter, noop.Int64Counter{},
			"genai.tool.calls",
			"Failed to create tool call counter, metrics will be disabled", meterName,
			metric.WithDescription("The number of tool calls made during execution"),
			metric.WithUnit("{calls}")),
		// Bucket boundaries are defined by the OpenTelemetry GenAI semantic conventions.
		// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/
		semconvTokenUsage: newInstrument[metric.Float64Histogram, metric.Float64HistogramOption](meter.Float64Histogram, noop.Float64Histogram{},
			"gen_ai.client.token.usage",
			"Failed to create gen_ai.client.token.usage histogram, semconv metrics will be disabled", meterName,
			metric.WithDescription("Measures the number of input and output tokens used"),
			metric.WithUnit("{token}"),
			metric.WithExplicitBucketBoundaries(
				1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864,
			)),
		agentTurns: newInstrument[metric.Int64Histogram, metric.Int64HistogramOption](meter.Int64Histogram, noop.Int64Histogram{},
			"genai.agent.turns",
			"Failed to create agent turns histogram, metrics will be disabled", meterName,
			metric.WithDescription("Number of LLM round-trips (turns) consumed per agent execution"),
			metric.WithUnit("{turns}"),
			metric.WithExplicitBucketBoundaries(1, 5, 10, 20, 50, 100, 200, 500)),
		turnLimitExceeded: newInstrument[metric.Int64Counter, metric.Int64CounterOption](meter.Int64Counter, noop.Int64Counter{},
			"genai.agent.turn_limit_exceeded",
			"Failed to create turn limit exceeded counter, metrics will be disabled", meterName,
			metric.WithDescription("Number of agent executions aborted by the maxTurns hard limit"),
			metric.WithUnit("{executions}")),
		apiRequests: newInstrument[metric.Int64Counter, metric.Int64CounterOption](meter.Int64Counter, noop.Int64Counter{},
			"genai.api.requests",
			"Failed to create api requests counter, metrics will be disabled", meterName,
			metric.WithDescription("LLM API request attempts, labeled by HTTP response code"),
			metric.WithUnit("{requests}")),
	}
}

// enrich merges attributes carried on ctx onto base: the reconciler
// ExecutionContext (via EnrichAttributes) plus the agent name set by
// WithExecutionContext-adjacent WithDefaultAgentName / agentkit.WithAgentName.
// The agent name is emitted as the bounded agent_name dimension so every GenAI
// metric is filterable per agent without each agent wiring its own label. It is
// omitted when no name was set on ctx.
func enrich(ctx context.Context, base []attribute.KeyValue) []attribute.KeyValue {
	base = agenttrace.GetExecutionContext(ctx).EnrichAttributes(base)
	if name := agenttrace.GetDefaultAgentName(ctx); name != "" {
		base = append(base, attribute.String("agent_name", name))
	}
	return base
}

// recordAttrs assembles the attribute set shared by the Record* methods:
// method-specific base attributes first, enriched from the execution context,
// with caller-provided attrs appended last.
func recordAttrs(ctx context.Context, callerAttrs []attribute.KeyValue, base ...attribute.KeyValue) []attribute.KeyValue {
	return append(enrich(ctx, base), callerAttrs...)
}

// RecordTokens records prompt and completion token usage.
// Enriches attributes from the execution context propagated via context.Context.
func (m *GenAI) RecordTokens(ctx context.Context, model string, promptTokens, completionTokens int64, attrs ...attribute.KeyValue) {
	baseAttrs := recordAttrs(ctx, attrs,
		attribute.String("model", model),
		attribute.String("gen_ai.request.model", model))

	// Custom metrics (existing)
	m.promptTokens.Add(ctx, promptTokens, metric.WithAttributes(baseAttrs...))
	m.completionTokens.Add(ctx, completionTokens, metric.WithAttributes(baseAttrs...))

	// GenAI semconv: histogram with gen_ai.token.type dimension.
	// gen_ai.operation.name is Required per spec; callers should pass gen_ai.provider.name via attrs.
	semconvBase := append(slices.Clone(baseAttrs),
		attribute.String("gen_ai.operation.name", "invoke_agent"),
	)
	inputAttrs := append(slices.Clone(semconvBase), attribute.String("gen_ai.token.type", "input"))
	m.semconvTokenUsage.Record(ctx, float64(promptTokens), metric.WithAttributes(inputAttrs...))
	outputAttrs := append(slices.Clone(semconvBase), attribute.String("gen_ai.token.type", "output"))
	m.semconvTokenUsage.Record(ctx, float64(completionTokens), metric.WithAttributes(outputAttrs...))
}

// RecordCacheTokens records Anthropic prompt cache token usage on the existing
// prompt tokens counter using gen_ai.token.type dimension values "cache_read"
// and "cache_creation". This aligns with the OpenTelemetry GenAI semantic
// conventions pattern from gen_ai.client.token.usage (see #35633).
//
// cacheRead: tokens served from cache (cheap — 0.1x base input price).
// cacheCreation: tokens written to cache (1.25x base input price, amortized over reads).
//
// A healthy caching setup shows high cache_read and low/zero cache_creation
// after the first turn.
func (m *GenAI) RecordCacheTokens(ctx context.Context, model string, cacheRead, cacheCreation int64, attrs ...attribute.KeyValue) {
	// Unlike the other Record* methods, this deliberately omits
	// gen_ai.request.model: adding it would change the emitted metric series.
	baseAttrs := recordAttrs(ctx, attrs, attribute.String("model", model))

	semconvBase := append(slices.Clone(baseAttrs),
		attribute.String("gen_ai.operation.name", "invoke_agent"),
	)
	if cacheRead > 0 {
		readAttrs := append(slices.Clone(baseAttrs), attribute.String("gen_ai.token.type", "cache_read"))
		m.promptTokens.Add(ctx, cacheRead, metric.WithAttributes(readAttrs...))
		semconvReadAttrs := append(slices.Clone(semconvBase), attribute.String("gen_ai.token.type", "cache_read"))
		m.semconvTokenUsage.Record(ctx, float64(cacheRead), metric.WithAttributes(semconvReadAttrs...))
	}
	if cacheCreation > 0 {
		creationAttrs := append(slices.Clone(baseAttrs), attribute.String("gen_ai.token.type", "cache_creation"))
		m.promptTokens.Add(ctx, cacheCreation, metric.WithAttributes(creationAttrs...))
		semconvCreationAttrs := append(slices.Clone(semconvBase), attribute.String("gen_ai.token.type", "cache_creation"))
		m.semconvTokenUsage.Record(ctx, float64(cacheCreation), metric.WithAttributes(semconvCreationAttrs...))
	}
}

// RecordToolCall records a tool invocation.
// Enriches attributes from the execution context propagated via context.Context.
func (m *GenAI) RecordToolCall(ctx context.Context, model, toolName string, attrs ...attribute.KeyValue) {
	baseAttrs := recordAttrs(ctx, attrs,
		attribute.String("model", model),
		attribute.String("gen_ai.request.model", model),
		attribute.String("tool", toolName))

	m.toolCallCounter.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
}

// RecordAPIRequest counts a single LLM API attempt. Each retry inside an in-process
// retry loop should call this once with the responseCode it observed — successes
// pass "200", rate-limit failures pass "429", overloaded failures pass "529", and
// errors that don't carry an HTTP status pass "unknown". This shape mirrors
// serviceruntime.googleapis.com/api/request_count's response_code label so PromQL
// on this metric reads the same as MQL on the GCP-side equivalent.
func (m *GenAI) RecordAPIRequest(ctx context.Context, model, responseCode string, attrs ...attribute.KeyValue) {
	baseAttrs := recordAttrs(ctx, attrs,
		attribute.String("model", model),
		attribute.String("gen_ai.request.model", model),
		attribute.String("response_code", responseCode))

	m.apiRequests.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
}

// RecordTurns records the number of LLM round-trips consumed by an agent execution.
// It always records the turns histogram and, when limitExceeded is true, also
// increments the turn_limit_exceeded counter. Call this on every exit path of the
// executor conversation loop so p50/p95/p99 distributions are observable per service.
func (m *GenAI) RecordTurns(ctx context.Context, model string, turns int, limitExceeded bool, attrs ...attribute.KeyValue) {
	baseAttrs := recordAttrs(ctx, attrs,
		attribute.String("model", model),
		attribute.String("gen_ai.request.model", model))

	m.agentTurns.Record(ctx, int64(turns), metric.WithAttributes(baseAttrs...))
	if limitExceeded {
		m.turnLimitExceeded.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	}
}
