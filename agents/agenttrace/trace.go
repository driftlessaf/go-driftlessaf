/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ReasoningContent represents internal reasoning from an LLM
type ReasoningContent struct {
	Thinking string `json:"thinking"`
}

// ToolCall represents a single tool invocation within a trace
type ToolCall[T any] struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Params    map[string]any `json:"params"`
	Result    any            `json:"result"`
	Error     error          `json:"error,omitempty"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	trace     *Trace[T]      // Parent trace for auto-adding on completion
	mu        sync.Mutex     // Protects mutable fields
	ctx       context.Context
	span      oteltrace.Span
}

// LLMTurn represents a single LLM call within a trace
type LLMTurn[T any] struct {
	span    oteltrace.Span
	trace   *Trace[T]
	prevCtx context.Context
	once    sync.Once
}

// Trace represents a complete agent interaction from prompt to result.
//
// Trace implements json.Marshaler so it can be serialized directly.
// The custom MarshalJSON converts the Error field to a string and
// excludes unexported runtime handles (mutex, context, span).
// Serialization is intended to happen after Complete — at that point
// the trace is immutable and no lock is needed.
type Trace[T any] struct {
	ID          string             `json:"id"`
	OTelTraceID string             `json:"otel_trace_id,omitempty"`
	InputPrompt string             `json:"input_prompt"`
	ExecContext ExecutionContext   `json:"exec_context,omitempty"` // PR/commit metadata
	ToolCalls   []*ToolCall[T]     `json:"tool_calls"`
	Reasoning   []ReasoningContent `json:"reasoning,omitempty"`
	Result      T                  `json:"result"`
	Error       error              `json:"-"` // handled by MarshalJSON
	StartTime   time.Time          `json:"start_time"`
	EndTime     time.Time          `json:"end_time"`
	Metadata    map[string]any     `json:"metadata,omitempty"`

	// Token usage fields, populated by RecordTokenUsage / RecordCacheTokenUsage.
	Model               string `json:"model,omitempty"`
	InputTokens         int64  `json:"input_tokens,omitempty"`
	OutputTokens        int64  `json:"output_tokens,omitempty"`
	CacheReadTokens     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64  `json:"cache_creation_tokens,omitempty"`

	// mu protects mutable fields during the build-up phase (concurrent
	// tool calls). After Complete the trace is immutable.
	mu   sync.Mutex
	ctx  context.Context
	span oteltrace.Span
}

// newTrace creates a new trace for the given prompt. The context must
// already contain a tracer (via WithTracer or StartTrace); Complete
// will panic otherwise.
func newTrace[T any](ctx context.Context, prompt string) *Trace[T] {
	// Extract execution context from Go context
	execCtx := GetExecutionContext(ctx)

	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	// Add execution context as span attributes.
	// Include both custom attributes and OpenTelemetry GenAI semantic conventions (gen_ai.*).
	// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/
	spanAttrs := []oteltrace.SpanStartOption{
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			attribute.String("agent.prompt", prompt),
			attribute.String("gen_ai.operation.name", "invoke_agent"),
		),
	}
	if execCtx.ReconcilerKey != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("reconciler_key", execCtx.ReconcilerKey)))
	}
	if execCtx.ReconcilerType != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("reconciler_type", execCtx.ReconcilerType)))
	}
	if execCtx.CommitSHA != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("commit_sha", execCtx.CommitSHA)))
	}

	ctx, span := tr.Start(ctx, "invoke_agent", spanAttrs...)

	var otelTraceID string
	if sc := span.SpanContext(); sc.HasTraceID() {
		otelTraceID = sc.TraceID().String()
	}

	return &Trace[T]{
		ID:          generateTraceID(),
		OTelTraceID: otelTraceID,
		InputPrompt: prompt,
		ExecContext: execCtx,
		ToolCalls:   []*ToolCall[T]{},
		StartTime:   time.Now(),
		Metadata:    make(map[string]any),
		ctx:         ctx,
		span:        span,
	}
}

// BeginTurn starts a new LLM turn span as a child of the trace span.
// The trace context is updated so subsequent tool call spans are nested under
// this turn span. Call End() on the returned LLMTurn when the turn completes.
//
// Callers MUST call End() on the current turn before calling BeginTurn again.
// Overlapping turns corrupt the span hierarchy: the later End() restores a
// stale context, causing subsequent spans to be parented incorrectly.
func (t *Trace[T]) BeginTurn(turn int, modelName string) *LLMTurn[T] {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	t.mu.Lock()
	parentCtx := t.ctx
	t.mu.Unlock()

	newCtx, span := tr.Start(parentCtx, "chat "+modelName,
		oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.request.model", modelName),
			attribute.Int("driftlessaf.turn.index", turn),
		))

	t.mu.Lock()
	t.ctx = newCtx
	t.mu.Unlock()

	return &LLMTurn[T]{
		span:    span,
		trace:   t,
		prevCtx: parentCtx,
	}
}

// RecordTokens sets input/output token counts as span attributes on the turn span.
func (lt *LLMTurn[T]) RecordTokens(inputTokens, outputTokens int64) {
	if lt.span != nil {
		lt.span.SetAttributes(
			attribute.Int64("gen_ai.usage.input_tokens", inputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", outputTokens),
		)
	}
}

// End ends the turn span and restores the trace context to before the turn.
// It is idempotent: subsequent calls are no-ops.
func (lt *LLMTurn[T]) End() {
	lt.once.Do(func() {
		if lt.span != nil {
			lt.span.End()
		}
		lt.trace.mu.Lock()
		lt.trace.ctx = lt.prevCtx
		lt.trace.mu.Unlock()
	})
}

// StartToolCall starts a new tool call and returns it
func (t *Trace[T]) StartToolCall(id, name string, params map[string]any) *ToolCall[T] {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	spanAttrs := []oteltrace.SpanStartOption{
		oteltrace.WithAttributes(
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("tool.name", name),
			attribute.String("tool.id", id),
		),
	}

	if paramsJSON, err := json.Marshal(params); err == nil {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
			attribute.String("gen_ai.input.messages", string(paramsJSON)),
		))
	}

	if reasoning, ok := params["reasoning"].(string); ok && reasoning != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
			attribute.String("driftlessaf.tool.reasoning", reasoning),
		))
	}

	t.mu.Lock()
	parentCtx := t.ctx
	t.mu.Unlock()

	ctx, span := tr.Start(parentCtx, "execute_tool "+name, spanAttrs...)

	return &ToolCall[T]{
		ID:        id,
		Name:      name,
		Params:    params,
		StartTime: time.Now(),
		trace:     t,
		ctx:       ctx,
		span:      span,
	}
}

// RecordTokenUsage records model and token usage as span attributes for observability.
// This allows viewing token consumption directly in Cloud Trace without needing to
// cross-reference with metrics.
//
// Attributes are emitted in both custom format (model, tokens.input, tokens.output)
// and OpenTelemetry GenAI semantic conventions (gen_ai.request.model, gen_ai.usage.input_tokens,
// gen_ai.usage.output_tokens).
// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/
func (t *Trace[T]) RecordTokenUsage(model string, inputTokens, outputTokens int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.Model = model
	t.InputTokens = inputTokens
	t.OutputTokens = outputTokens

	if t.span != nil {
		// Update span name to follow semconv format: "{operation} {model}"
		t.span.SetName("invoke_agent " + model)
		t.span.SetAttributes(
			// Custom attributes (existing)
			attribute.String("model", model),
			attribute.Int64("tokens.input", inputTokens),
			attribute.Int64("tokens.output", outputTokens),
			attribute.Int64("tokens.total", inputTokens+outputTokens),
			// OpenTelemetry GenAI semantic conventions
			attribute.String("gen_ai.request.model", model),
			attribute.Int64("gen_ai.usage.input_tokens", inputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", outputTokens),
		)
	}
}

// RecordCacheTokenUsage records Anthropic prompt cache token metrics as span
// attributes using OpenTelemetry GenAI semantic conventions. These appear
// alongside gen_ai.usage.input_tokens and gen_ai.usage.output_tokens in Cloud
// Trace, enabling per-request visibility into how much of the input was served
// from cache vs fresh.
// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/
func (t *Trace[T]) RecordCacheTokenUsage(cacheReadTokens, cacheCreationTokens int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.CacheReadTokens = cacheReadTokens
	t.CacheCreationTokens = cacheCreationTokens

	if t.span != nil {
		t.span.SetAttributes(
			// Custom attributes
			attribute.Int64("tokens.cache_read", cacheReadTokens),
			attribute.Int64("tokens.cache_creation", cacheCreationTokens),
			// OpenTelemetry GenAI semantic conventions
			attribute.Int64("gen_ai.usage.cache_read_input_tokens", cacheReadTokens),
			attribute.Int64("gen_ai.usage.cache_creation_input_tokens", cacheCreationTokens),
		)
	}
}

// BadToolCall records a tool call that failed due to bad arguments or unknown tool
func (t *Trace[T]) BadToolCall(id, name string, params map[string]any, err error) {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))
	t.mu.Lock()
	parentCtx := t.ctx
	t.mu.Unlock()

	_, span := tr.Start(parentCtx, "execute_tool "+name, oteltrace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("tool.name", name),
		attribute.String("tool.id", id),
		attribute.String("error", err.Error()),
	))
	span.SetStatus(codes.Error, err.Error())
	span.End()

	tc := &ToolCall[T]{
		ID:        id,
		Name:      name,
		Params:    params,
		StartTime: time.Now(),
		EndTime:   time.Now(),
		Error:     err,
		trace:     t,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.ToolCalls = append(t.ToolCalls, tc)
}

// Complete marks the tool call as complete and adds it to the parent trace
func (tc *ToolCall[T]) Complete(result any, err error) {
	tc.mu.Lock()
	tc.Result = result
	tc.Error = err
	tc.EndTime = time.Now()
	trace := tc.trace
	span := tc.span
	tc.mu.Unlock()

	if span != nil {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			if result != nil {
				if resultJSON, marshalErr := json.Marshal(result); marshalErr == nil {
					span.SetAttributes(attribute.String("gen_ai.output.messages", string(resultJSON)))
				}
			}
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}

	// Auto-add to parent trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.ToolCalls = append(trace.ToolCalls, tc)
}

// Duration returns the duration of the tool call
func (tc *ToolCall[T]) Duration() time.Duration {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.EndTime.IsZero() {
		return time.Since(tc.StartTime)
	}
	return tc.EndTime.Sub(tc.StartTime)
}

// complete marks the trace as complete with the given result. It fills in the
// result, error, and end time, and ends the OpenTelemetry span. It does NOT
// record the trace — that is the caller's responsibility (typically via the
// done callback returned by StartTrace).
func (t *Trace[T]) complete(result T, err error) {
	t.mu.Lock()
	t.Result = result
	t.Error = err
	t.EndTime = time.Now()
	span := t.span
	t.mu.Unlock()

	if span != nil {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// Duration returns the total duration of the trace
func (t *Trace[T]) Duration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.EndTime.IsZero() {
		return time.Since(t.StartTime)
	}
	return t.EndTime.Sub(t.StartTime)
}

// String returns a structured representation of the trace
func (t *Trace[T]) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var sb strings.Builder

	// Calculate duration while we have the lock
	var duration time.Duration
	if t.EndTime.IsZero() {
		duration = time.Since(t.StartTime)
	} else {
		duration = t.EndTime.Sub(t.StartTime)
	}

	// Header
	fmt.Fprintf(&sb, "=== Trace %s ===\n", t.ID)
	fmt.Fprintf(&sb, "Prompt: %q\n", t.InputPrompt)
	fmt.Fprintf(&sb, "Duration: %v\n", duration)

	// Reasoning
	if len(t.Reasoning) > 0 {
		fmt.Fprintf(&sb, "\nReasoning (%d blocks):\n", len(t.Reasoning))
		for i, r := range t.Reasoning {
			thinkingStr := r.Thinking
			if len(thinkingStr) > 200 {
				thinkingStr = thinkingStr[:197] + "..."
			}
			fmt.Fprintf(&sb, "  [%d] %s\n", i+1, thinkingStr)
		}
	}

	// Tool calls
	if len(t.ToolCalls) > 0 {
		fmt.Fprintf(&sb, "\nTool Calls (%d):\n", len(t.ToolCalls))
		for i, tc := range t.ToolCalls {
			fmt.Fprintf(&sb, "  [%d] %s (ID: %s)\n", i+1, tc.Name, tc.ID)

			// Calculate tool call duration inline to avoid nested mutex lock
			var tcDuration time.Duration
			if tc.EndTime.IsZero() {
				tcDuration = time.Since(tc.StartTime)
			} else {
				tcDuration = tc.EndTime.Sub(tc.StartTime)
			}
			fmt.Fprintf(&sb, "      Duration: %v\n", tcDuration)

			// Parameters
			if len(tc.Params) > 0 {
				sb.WriteString("      Params:\n")
				for k, v := range tc.Params {
					fmt.Fprintf(&sb, "        %s: %v\n", k, v)
				}
			}

			// Result/Error
			if tc.Error != nil {
				fmt.Fprintf(&sb, "      Error: %v\n", tc.Error)
			} else if tc.Result != nil {
				// Limit result output to avoid huge logs
				resultStr := fmt.Sprintf("%v", tc.Result)
				if len(resultStr) > 200 {
					resultStr = resultStr[:197] + "..."
				}
				fmt.Fprintf(&sb, "      Result: %s\n", resultStr)
			}
		}
	} else {
		sb.WriteString("\nNo tool calls\n")
	}

	// Final result/error
	sb.WriteString("\nCompletion:\n")
	switch {
	case t.Error != nil:
		fmt.Fprintf(&sb, "  Error: %v\n", t.Error)
	case any(t.Result) != nil:
		// Limit result output
		resultStr := fmt.Sprintf("%v", t.Result)
		if len(resultStr) > 500 {
			resultStr = resultStr[:497] + "..."
		}
		fmt.Fprintf(&sb, "  Result: %s\n", resultStr)
	default:
		sb.WriteString("  Result: <nil>\n")
	}

	// Metadata if present
	if len(t.Metadata) > 0 {
		sb.WriteString("\nMetadata:\n")
		for k, v := range t.Metadata {
			fmt.Fprintf(&sb, "  %s: %v\n", k, v)
		}
	}

	return sb.String()
}

// MarshalJSON implements json.Marshaler for Trace.
// It converts the Error field to a string and excludes unexported fields.
func (t *Trace[T]) MarshalJSON() ([]byte, error) {
	type jsonToolCall struct {
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Params    map[string]any `json:"params,omitempty"`
		Result    any            `json:"result,omitempty"`
		Error     string         `json:"error,omitempty"`
		StartTime time.Time      `json:"start_time"`
		EndTime   time.Time      `json:"end_time"`
	}

	toolCalls := make([]jsonToolCall, len(t.ToolCalls))
	for i, tc := range t.ToolCalls {
		jtc := jsonToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Params:    tc.Params,
			Result:    tc.Result,
			StartTime: tc.StartTime,
			EndTime:   tc.EndTime,
		}
		if tc.Error != nil {
			jtc.Error = tc.Error.Error()
		}
		toolCalls[i] = jtc
	}

	var errStr string
	if t.Error != nil {
		errStr = t.Error.Error()
	}

	return json.Marshal(struct {
		ID                  string             `json:"id"`
		OTelTraceID         string             `json:"otel_trace_id,omitempty"`
		InputPrompt         string             `json:"input_prompt"`
		ExecContext         ExecutionContext   `json:"exec_context,omitempty"`
		ToolCalls           []jsonToolCall     `json:"tool_calls"`
		Reasoning           []ReasoningContent `json:"reasoning,omitempty"`
		Result              T                  `json:"result"`
		Error               string             `json:"error,omitempty"`
		StartTime           time.Time          `json:"start_time"`
		EndTime             time.Time          `json:"end_time"`
		Metadata            map[string]any     `json:"metadata,omitempty"`
		Model               string             `json:"model,omitempty"`
		InputTokens         int64              `json:"input_tokens,omitempty"`
		OutputTokens        int64              `json:"output_tokens,omitempty"`
		CacheReadTokens     int64              `json:"cache_read_tokens,omitempty"`
		CacheCreationTokens int64              `json:"cache_creation_tokens,omitempty"`
	}{
		ID:                  t.ID,
		OTelTraceID:         t.OTelTraceID,
		InputPrompt:         t.InputPrompt,
		ExecContext:         t.ExecContext,
		ToolCalls:           toolCalls,
		Reasoning:           t.Reasoning,
		Result:              t.Result,
		Error:               errStr,
		StartTime:           t.StartTime,
		EndTime:             t.EndTime,
		Metadata:            t.Metadata,
		Model:               t.Model,
		InputTokens:         t.InputTokens,
		OutputTokens:        t.OutputTokens,
		CacheReadTokens:     t.CacheReadTokens,
		CacheCreationTokens: t.CacheCreationTokens,
	})
}

// generateTraceID generates a unique trace ID
func generateTraceID() string {
	// Generate a random component
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp only if random generation fails
		return time.Now().Format("20060102-150405.000000")
	}
	// Format: YYYYMMDD-HHMMSS-RRRR where RRRR is random hex
	return fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(b))
}
