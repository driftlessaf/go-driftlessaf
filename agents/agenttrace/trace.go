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

// maxPayloadBytes is the maximum byte length for a payload attribute emitted
// on the root invoke_agent span. Both Braintrust and Langfuse ingest reject
// ~1 MB+ batches, so fixer prompts / completions that exceed this get
// truncated with a braintrust.metadata.truncated=true marker.
const maxPayloadBytes = 64 * 1024

// Canonical gen_ai.system values per the OpenTelemetry GenAI semantic
// conventions. Executors pass these to BeginTurn so downstream eval tools
// (Braintrust, Langfuse, …) can filter traces by provider without
// consumers needing to remember the exact spelling.
const (
	SystemAnthropic    = "anthropic"
	SystemGoogleVertex = "google.vertex"
	SystemOpenAI       = "openai"
)

// StartTraceOption configures trace creation. Options let callers attach
// an agent name (static, for gen_ai.agent.name) and a dynamic name function
// (for braintrust.span_attributes.name — e.g. "autofix: pr:chainguard-dev/mono#38632").
type StartTraceOption func(*traceOptions)

type traceOptions struct {
	agentName string
	nameFn    func(ExecutionContext) string
}

// WithAgentName sets the static gen_ai.agent.name attribute on the root
// invoke_agent span (e.g. "loganalyzer", "judge", "fixer"). Also used as
// the fallback for braintrust.span_attributes.name when no nameFn is set.
func WithAgentName(name string) StartTraceOption {
	return func(o *traceOptions) {
		o.agentName = name
	}
}

// WithNameFn sets a callback that produces the braintrust.span_attributes.name
// attribute (the label shown in the Braintrust UI) from the ExecutionContext.
// When nil or unset, braintrust.span_attributes.name falls back to agentName.
func WithNameFn(fn func(ExecutionContext) string) StartTraceOption {
	return func(o *traceOptions) {
		o.nameFn = fn
	}
}

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

// LLMTurn represents a single LLM call within a trace.
//
// LLMTurn is not safe for concurrent use: callers own a turn for the duration
// of a single model roundtrip and must not hand it across goroutines. The
// lifecycle methods (RecordTokens, End) read and mutate the accumulated record
// without locking because the contract is single-goroutine-per-turn, mirroring
// ToolCall.
type LLMTurn[T any] struct {
	span    oteltrace.Span
	trace   *Trace[T]
	prevCtx context.Context
	once    sync.Once

	// record accumulates per-turn data appended to trace.Turns on End.
	record RecordedTurn
}

// RecordedTurn is the per-turn data captured on a Trace so each LLM turn
// surfaces as a queryable row in downstream sinks (e.g. BigQuery via the
// CloudEvent payload). Token counts reflect a single turn rather than
// cumulative totals on the parent Trace.
type RecordedTurn struct {
	Index        int       `json:"index"`
	Model        string    `json:"model,omitempty"`
	System       string    `json:"system,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	// Error is reserved for per-turn completion detail. Nothing populates it
	// today — End() takes no error argument. A follow-up will wire turn-level
	// error capture (e.g. retry-exhaustion, malformed response) into this field.
	Error string `json:"error,omitempty"`
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
	Turns       []RecordedTurn     `json:"turns,omitempty"`
	Reasoning   []ReasoningContent `json:"reasoning,omitempty"`
	Result      T                  `json:"result"`
	Error       error              `json:"-"` // handled by MarshalJSON
	StartTime   time.Time          `json:"start_time"`
	EndTime     time.Time          `json:"end_time"`
	Metadata    map[string]any     `json:"metadata,omitempty"`

	// AgentName identifies which logical agent produced this trace (e.g.
	// "materializer", "skillup-reviewer"). Stamped by a middleware that
	// knows the agent identity at tracer construction time.
	AgentName string `json:"agent_name,omitempty"`

	// Source mirrors the CloudEvent Ce-Source header (typically the
	// reconciler's OCTO_IDENTITY). Duplicated into the payload so BigQuery
	// can query by service — the recorder records only event.Data().
	Source string `json:"source,omitempty"`

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
func newTrace[T any](ctx context.Context, prompt string, opts ...StartTraceOption) *Trace[T] {
	// Seed options from context defaults so a reconciler that wrapped the
	// context with WithDefaultAgentName / WithDefaultNameFn gets its naming
	// applied to every executor-driven trace without threading options
	// through every intermediate call site. Explicit opts below still win.
	o := traceOptions{
		agentName: GetDefaultAgentName(ctx),
		nameFn:    GetDefaultNameFn(ctx),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	// Extract execution context from Go context
	execCtx := GetExecutionContext(ctx)

	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	// Compute the Braintrust-facing span name. braintrust.span_attributes.name
	// drives the label in the Braintrust UI. nameFn wins for PR-aware labels
	// ("autofix: pr:chainguard-dev/mono#38632"); otherwise fall back to the
	// static agent name.
	btName := o.agentName
	if o.nameFn != nil {
		if n := o.nameFn(execCtx); n != "" {
			btName = n
		}
	}

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
	if o.agentName != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("gen_ai.agent.name", o.agentName)))
	}
	if btName != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("braintrust.span_attributes.name", btName)))
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

	// Payload emission (gen_ai.prompt + gen_ai.input.messages) is gated on
	// the WithPayloadsEnabled ctx opt-in. Staging can opt in; prod stays off
	// pending security review because prompts may contain build-log tokens
	// and internal URLs. Dual-emit both the Langfuse-compatible (gen_ai.prompt)
	// and Braintrust / OTel-semconv-compatible (gen_ai.input.messages) keys
	// so either backend picks up the payload with zero code revisit.
	if payloadsEnabledFrom(ctx) && prompt != "" {
		if payload, err := json.Marshal([]map[string]string{{"role": "user", "content": prompt}}); err == nil {
			payloadStr, truncated := truncatePayload(string(payload))
			spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
				attribute.String("gen_ai.prompt", payloadStr),
				attribute.String("gen_ai.input.messages", payloadStr),
			))
			if truncated {
				spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
					attribute.Bool("braintrust.metadata.truncated", true),
				))
			}
		}
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
		// Derive the payload-side agent name from the same o.agentName the
		// gen_ai.agent.name span attr uses, so BQ rows and OTel spans always
		// agree. o.agentName is seeded from WithDefaultAgentName(ctx) and
		// overridden by any explicit WithAgentName opt.
		AgentName: o.agentName,
		ctx:       ctx,
		span:      span,
	}
}

// truncatePayload truncates a payload string to maxPayloadBytes and reports
// whether truncation occurred. Byte-based (not rune-based) because the
// constraint is on the OTLP batch size, not logical content.
func truncatePayload(s string) (string, bool) {
	if len(s) <= maxPayloadBytes {
		return s, false
	}
	return s[:maxPayloadBytes], true
}

// BeginTurn starts a new LLM turn span as a child of the trace span.
// The trace context is updated so subsequent tool call spans are nested under
// this turn span. Call End() on the returned LLMTurn when the turn completes.
//
// system is the OTel GenAI provider identifier: "openai", "anthropic",
// "google.vertex", etc. It powers provider filtering in eval tools.
//
// Callers MUST call End() on the current turn before calling BeginTurn again.
// Overlapping turns corrupt the span hierarchy: the later End() restores a
// stale context, causing subsequent spans to be parented incorrectly.
func (t *Trace[T]) BeginTurn(turn int, system, modelName string) *LLMTurn[T] {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	t.mu.Lock()
	parentCtx := t.ctx
	t.mu.Unlock()

	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", modelName),
		attribute.Int("driftlessaf.turn.index", turn),
	}
	if system != "" {
		attrs = append(attrs, attribute.String("gen_ai.system", system))
	}

	newCtx, span := tr.Start(parentCtx, "chat "+modelName,
		oteltrace.WithAttributes(attrs...))

	t.mu.Lock()
	t.ctx = newCtx
	t.mu.Unlock()

	return &LLMTurn[T]{
		span:    span,
		trace:   t,
		prevCtx: parentCtx,
		record: RecordedTurn{
			Index:     turn,
			Model:     modelName,
			System:    system,
			StartTime: time.Now(),
		},
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
	lt.record.InputTokens = inputTokens
	lt.record.OutputTokens = outputTokens
}

// End ends the turn span and restores the trace context to before the turn.
// It is idempotent: subsequent calls are no-ops. On the first call, it
// appends the accumulated RecordedTurn to the parent trace's Turns slice.
func (lt *LLMTurn[T]) End() {
	lt.once.Do(func() {
		if lt.span != nil {
			lt.span.End()
		}
		lt.record.EndTime = time.Now()
		lt.trace.mu.Lock()
		lt.trace.ctx = lt.prevCtx
		lt.trace.Turns = append(lt.trace.Turns, lt.record)
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
// Token counts are emitted on the root invoke_agent span via OpenTelemetry GenAI
// semantic conventions (gen_ai.usage.input_tokens / gen_ai.usage.output_tokens)
// alongside legacy custom attributes. gen_ai.request.model is NOT emitted on the
// root span — per OTel GenAI semconv it's a turn-level concern, emitted on the
// "chat <model>" spans in BeginTurn. Langfuse reclassifies any root span carrying
// gen_ai.request.model as a "generation" observation, which is wrong for an
// orchestration span; Braintrust doesn't reclassify but semantically agrees.
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
			// OpenTelemetry GenAI semantic conventions (token usage only —
			// model is a turn-level concern, emitted in BeginTurn)
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
		// Emit the final result payload on the root invoke_agent span before
		// ending it. Dual-emit both the Langfuse-compatible (gen_ai.completion)
		// and Braintrust / OTel-semconv-compatible (gen_ai.output.messages)
		// keys so either backend auto-populates the output field. Gated on
		// the WithPayloadsEnabled ctx opt-in — same reasoning as prompt emission.
		if payloadsEnabledFrom(t.ctx) && err == nil {
			if payload, mErr := json.Marshal(result); mErr == nil && len(payload) > 0 && string(payload) != "null" {
				payloadStr, truncated := truncatePayload(string(payload))
				span.SetAttributes(
					attribute.String("gen_ai.completion", payloadStr),
					attribute.String("gen_ai.output.messages", payloadStr),
				)
				if truncated {
					span.SetAttributes(attribute.Bool("braintrust.metadata.truncated", true))
				}
			}
		}
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
		Turns               []RecordedTurn     `json:"turns,omitempty"`
		Reasoning           []ReasoningContent `json:"reasoning,omitempty"`
		Result              T                  `json:"result"`
		Error               string             `json:"error,omitempty"`
		StartTime           time.Time          `json:"start_time"`
		EndTime             time.Time          `json:"end_time"`
		Metadata            map[string]any     `json:"metadata,omitempty"`
		AgentName           string             `json:"agent_name,omitempty"`
		Source              string             `json:"source,omitempty"`
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
		Turns:               t.Turns,
		Reasoning:           t.Reasoning,
		Result:              t.Result,
		Error:               errStr,
		StartTime:           t.StartTime,
		EndTime:             t.EndTime,
		Metadata:            t.Metadata,
		AgentName:           t.AgentName,
		Source:              t.Source,
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
