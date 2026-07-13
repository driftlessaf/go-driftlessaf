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

	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// maxPayloadBytes is the maximum byte length for a payload attribute emitted
// on the root invoke_agent span. Most OTLP ingest endpoints reject ~1 MB+
// batches, so fixer prompts / completions that exceed this get truncated
// with a driftlessaf.payload.truncated=true marker.
const maxPayloadBytes = 64 * 1024

// Canonical gen_ai.system values per the OpenTelemetry GenAI semantic
// conventions. Executors pass these to BeginTurn so downstream eval tools
// can filter traces by provider without consumers needing to remember the
// exact spelling.
const (
	SystemAnthropic    = "anthropic"
	SystemGoogleVertex = "google.vertex"
	SystemOpenAI       = "openai"
)

// StartTraceOption configures trace creation. Options let callers attach
// an agent name (static, for gen_ai.agent.name) and a dynamic name function
// (for driftlessaf.invocation.label — e.g. "autofix: pr:chainguard-dev/mono#38632").
type StartTraceOption func(*traceOptions)

type traceOptions struct {
	agentName string
	nameFn    func(ExecutionContext) string
}

// WithAgentName sets the static gen_ai.agent.name attribute on the root
// invoke_agent span (e.g. "loganalyzer", "judge", "fixer"). Also used as
// the fallback for driftlessaf.invocation.label when no nameFn is set.
func WithAgentName(name string) StartTraceOption {
	return func(o *traceOptions) {
		o.agentName = name
	}
}

// WithNameFn sets a callback that produces the driftlessaf.invocation.label
// attribute — a per-invocation, free-form label (e.g.
// "autofix: pr:chainguard-dev/mono#38632") computed from the
// ExecutionContext. Vendor-neutral; backends that want to use this as the
// primary trace name (Braintrust, Langfuse, etc.) bridge it in user-side
// span processors. When nil or unset, driftlessaf.invocation.label falls
// back to agentName.
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

	// requestPayload and responsePayload are populated by RecordRequest /
	// RecordResponse when WithPayloadsEnabled is set. Used by End to emit a
	// per-span CloudEvent through trace.spanEmitter.
	requestPayload  []byte
	responsePayload []byte

	// requestTruncated / responseTruncated record whether preparePayload
	// shortened the corresponding payload to fit inside maxPayloadBytes.
	// Either flag set surfaces as driftlessaf.payload.truncated = true on
	// the span metadata so downstream consumers can distinguish a real
	// prompt body from a truncated one.
	requestTruncated  bool
	responseTruncated bool
}

// RecordedTurn is the per-turn data captured on a Trace so each LLM turn
// surfaces as a queryable row in downstream sinks (e.g. BigQuery via the
// CloudEvent payload). Token counts reflect a single turn rather than
// cumulative totals on the parent Trace.
type RecordedTurn struct {
	Index               int       `json:"index"`
	Model               string    `json:"model,omitempty"`
	System              string    `json:"system,omitempty"`
	InputTokens         int64     `json:"input_tokens,omitempty"`
	OutputTokens        int64     `json:"output_tokens,omitempty"`
	CacheReadTokens     int64     `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64     `json:"cache_creation_tokens,omitempty"`
	StartTime           time.Time `json:"start_time"`
	EndTime             time.Time `json:"end_time"`
	// Errors is the chronological list of errors the turn encountered, including
	// transients that the turn recovered from. A non-empty list does NOT mean
	// the turn failed — see Failed for the terminal outcome. Populated via
	// LLMTurn.RecordError and LLMTurn.Fail.
	Errors []string `json:"errors,omitempty"`
	// Failed is the terminal outcome flag: true iff the turn ultimately failed.
	// A turn can have non-empty Errors and Failed=false (it recovered from
	// transient errors). Set by LLMTurn.Fail; defaults to false. Serialized
	// without omitempty so successful turns get an explicit `false` in BQ
	// instead of NULL — analytics queries can use `failed = FALSE` directly
	// without the three-valued-logic gotcha of NULL.
	Failed bool `json:"failed"`
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

	// Model identifies the LLM the executor drove this trace against
	// (e.g. "claude-sonnet-4-6", "gemini-2.5-pro"). Populated lazily by
	// BeginTurn from the first turn's model — assumes a single-model
	// trace, which matches every executor in the codebase. Per-turn
	// model lives on Turns[i].Model so multi-model traces can still be
	// reasoned about turn-by-turn.
	Model string `json:"model,omitempty"`

	// mu protects mutable fields during the build-up phase (concurrent
	// tool calls). After Complete the trace is immutable.
	mu   sync.Mutex
	ctx  context.Context
	span oteltrace.Span

	// spanEmitter, when non-nil, receives a RecordedSpan from LLMTurn.End
	// for every turn that recorded a prompt payload. Wired by the
	// CloudEvent-emitting tracer; nil for the default tracer.
	spanEmitter SpanEmitter
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

	// Compute the per-invocation label emitted as driftlessaf.invocation.label.
	// nameFn wins for resource-aware labels ("autofix: pr:chainguard-dev/mono#38632");
	// otherwise fall back to the static agent name.
	invocationLabel := o.agentName
	if o.nameFn != nil {
		if n := o.nameFn(execCtx); n != "" {
			invocationLabel = n
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
	if invocationLabel != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("driftlessaf.invocation.label", invocationLabel)))
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
	if execCtx.RequestID != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("request_id", execCtx.RequestID)))
	}

	// Payload emission (gen_ai.prompt + gen_ai.input.messages) is gated on
	// the WithPayloadsEnabled ctx opt-in. Staging can opt in; prod stays off
	// pending security review because prompts may contain build-log tokens
	// and internal URLs. Both gen_ai.prompt and gen_ai.input.messages are
	// emitted so backends that read either OTel-semconv variant of the
	// payload attribute pick it up.
	if payloadsEnabledFrom(ctx) && prompt != "" {
		if payload, err := json.Marshal([]map[string]string{{"role": "user", "content": prompt}}); err == nil {
			payloadStr, truncated := truncatePayload(string(payload))
			spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
				attribute.String("gen_ai.prompt", payloadStr),
				attribute.String("gen_ai.input.messages", payloadStr),
			))
			if truncated {
				spanAttrs = append(spanAttrs, oteltrace.WithAttributes(
					attribute.Bool("driftlessaf.payload.truncated", true),
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

// Context returns the context the trace was created with. Eval callbacks
// run after Complete() and historically derived from context.Background()
// to detach from any cancellation/deadline on the original request, but
// that also dropped the reconciler's WithDefaultNameFn / WithDefaultAgentName
// / WithPayloadsEnabled, causing every eval-emitted trace (e.g. judge) to
// surface as an orphan root span with no link to the parent trace tree.
// Callbacks should wrap the returned ctx with context.WithoutCancel before
// kicking off long-lived work so they inherit the reconciler-set values
// without inheriting the completed request's cancellation.
func (t *Trace[T]) Context() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctx
}

// BeginTurn starts a new LLM turn span as a child of the trace span.
// The trace context is updated so subsequent tool call spans are nested under
// this turn span. Call End() on the returned LLMTurn when the turn completes.
//
// system is the OTel GenAI provider identifier: "openai", "anthropic",
// "google.vertex", etc. It powers provider filtering in eval tools.
//
// Per-call token usage (input/output and prompt cache) is recorded by
// LLMTurn.RecordTokens / LLMTurn.RecordCacheTokens — replacing the old
// trace-level RecordTokenUsage / RecordCacheTokenUsage methods removed
// in DEV-1140. Per OTel GenAI semconv, gen_ai.usage.* attributes belong
// on the per-call "chat <model>" span, not the orchestration invoke_agent
// span. Trace-level token totals are derived from turns[] in downstream
// consumers (see agent_trace_costs.sql).
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
	// Latch the trace-level model from the first turn that names one. The
	// root span name follows OTel GenAI semconv "{operation} {model}" — the
	// model isn't known when the trace starts, so we set it here.
	if t.Model == "" && modelName != "" {
		t.Model = modelName
		if t.span != nil {
			t.span.SetName("invoke_agent " + modelName)
		}
	}
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

// RecordCacheTokens sets prompt cache token counts as span attributes on the
// turn span and on the per-turn record. The OTel GenAI semconv attributes
// (gen_ai.usage.cache_read_input_tokens, gen_ai.usage.cache_creation_input_tokens)
// belong on the per-call chat span, not the orchestration span. Per-turn
// cache counts let downstream cost analysis apply the cache discount /
// surcharge accurately on a per-call basis.
func (lt *LLMTurn[T]) RecordCacheTokens(cacheReadTokens, cacheCreationTokens int64) {
	if lt.span != nil {
		lt.span.SetAttributes(
			attribute.Int64("gen_ai.usage.cache_read_input_tokens", cacheReadTokens),
			attribute.Int64("gen_ai.usage.cache_creation_input_tokens", cacheCreationTokens),
		)
	}
	lt.record.CacheReadTokens = cacheReadTokens
	lt.record.CacheCreationTokens = cacheCreationTokens
}

// RecordError appends err to the turn's chronological error list and emits
// an exception event on the OTEL span. It does NOT mark the turn as failed —
// use Fail for that. RecordError is the right call for transient errors the
// turn recovered from (e.g. a 503 that succeeded on retry); a non-empty
// Errors list with Failed=false is exactly that recovery shape.
//
// A nil err is a no-op. Call before End.
func (lt *LLMTurn[T]) RecordError(err error) {
	if err == nil {
		return
	}
	if lt.span != nil {
		lt.span.RecordError(err)
	}
	lt.record.Errors = append(lt.record.Errors, err.Error())
}

// Fail marks the turn as having ultimately failed and sets the OTEL span
// status to Error — mirroring ToolCall.Complete and Trace.complete on the
// failure path. RecordedTurn.Failed flips to true unconditionally; if err is
// non-nil it is also appended to Errors and recorded as a span exception
// event.
//
// Fail(nil) is intentionally NOT symmetric with RecordError(nil). Calling
// Fail means "this turn ended in failure" — that signal must propagate even
// when the caller has no concrete error value (e.g. context cancellation
// surfaced upstream as a sentinel). The alternative (silent no-op) trades a
// loud false positive for a silent loss of failure signal in BQ; we prefer
// the loud one because it's discoverable. Callers that don't want to fail
// the turn must guard the call themselves: `if err != nil { lt.Fail(err) }`,
// which is exactly what the executor wiring does.
//
// Call before End. Safe to call multiple times: the Failed flag is sticky
// (subsequent calls don't toggle it off), and each call with non-nil err
// appends to Errors.
func (lt *LLMTurn[T]) Fail(err error) {
	lt.record.Failed = true
	if err != nil {
		lt.record.Errors = append(lt.record.Errors, err.Error())
	}
	if lt.span != nil {
		desc := ""
		if err != nil {
			lt.span.RecordError(err)
			desc = err.Error()
		}
		lt.span.SetStatus(codes.Error, desc)
	}
}

// End ends the turn span and restores the trace context to before the turn.
// It is idempotent: subsequent calls are no-ops. On the first call, it
// appends the accumulated RecordedTurn to the parent trace's Turns slice
// and, when a request payload was recorded and the trace has a spanEmitter
// configured, emits a per-span CloudEvent for the turn.
func (lt *LLMTurn[T]) End() {
	lt.once.Do(func() {
		if lt.span != nil {
			lt.span.End()
		}
		lt.record.EndTime = time.Now()
		lt.trace.mu.Lock()
		lt.trace.ctx = lt.prevCtx
		lt.trace.Turns = append(lt.trace.Turns, lt.record)
		emitter := lt.trace.spanEmitter
		emitCtx := lt.trace.ctx
		lt.trace.mu.Unlock()
		// Decouple lt.record.Errors from the slice header just appended into
		// trace.Turns. The contract says "Call before End", but a violation
		// would otherwise produce capacity-dependent behavior — a post-End
		// RecordError might mutate the parent's backing array (if cap > len)
		// or be silently lost (if cap == len). After this nil-out, lt.record
		// owns no shared array; any post-End append allocates fresh and
		// writes into a private grave.
		lt.record.Errors = nil

		// localSink is the type-erased process-global sink (SetLocalSpanSink),
		// invoked in addition to the per-tracer emitter so an in-process
		// consumer that cannot register a per-response-type tracer (a CLI
		// writing a local telemetry file across many agent types) still
		// receives every turn. Build the span once and fan out to both.
		localSink := currentLocalSpanSink()
		if emitter != nil || localSink != nil {
			if span, ok := lt.buildRecordedSpan(); ok {
				// Emitters are expected to be non-blocking and to handle their
				// own delivery retries (see ceEmittingTracer.emitSpan). The
				// only way one returns a non-nil error here is a synchronous
				// pre-flight failure such as ce.SetData rejecting the span
				// (the bug fixed in #3303142362). Log it at the call site so
				// silent span loss is observable in Cloud Run logs without
				// forcing End() to propagate the error — End() is the cleanup
				// path on a deferred turn and has no useful return channel.
				if emitter != nil {
					if err := emitter(emitCtx, span); err != nil {
						clog.WarnContext(emitCtx, "agent span emit dropped",
							"trace_id", lt.trace.ID,
							"span_id", span.SpanID,
							"error", err.Error(),
						)
					}
				}
				if localSink != nil {
					if err := localSink(emitCtx, span); err != nil {
						clog.WarnContext(emitCtx, "agent local span sink dropped",
							"trace_id", lt.trace.ID,
							"span_id", span.SpanID,
							"error", err.Error(),
						)
					}
				}
			}
		}
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

// AttachToolCallReasoning merges the model-supplied reasoning for the tool
// call with the given id into that call's recorded params. Executors call
// this after dispatching a tool: the model passes a universal `reasoning`
// argument on every call (auto-injected into each tool schema by the
// executor bridges), but handlers record curated param maps that drop it.
// Attaching it here preserves the per-action rationale on the trace record —
// and, since params serialize verbatim, in BigQuery's tool_calls[].params
// JSON. A no-op when no completed call matches id (e.g. the handler bailed
// on a parameter error before recording) or when the handler already
// recorded a reasoning value.
func (t *Trace[T]) AttachToolCallReasoning(id, reasoning string) {
	if reasoning == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.ToolCalls) - 1; i >= 0; i-- {
		tc := t.ToolCalls[i]
		if tc.ID != id {
			continue
		}
		tc.mu.Lock()
		if tc.Params == nil {
			tc.Params = map[string]any{}
		}
		if _, exists := tc.Params["reasoning"]; !exists {
			tc.Params["reasoning"] = reasoning
		}
		tc.mu.Unlock()
		return
	}
}

// AppendReasoning records a block of raw model reasoning (Claude
// thinking / redacted_thinking blocks, Gemini thought parts) on the trace,
// but only when the WithPayloadsEnabled opt-in is set on the trace context.
// Model reasoning is unredacted completion content: under the Derivative
// Sensitivity Inheritance invariant it inherits the parent submission's
// confidentiality tier, so it is gated behind the same payload opt-in as
// raw prompt and completion emission rather than being captured
// unconditionally. A no-op when the content is empty or the opt-in is off.
func (t *Trace[T]) AppendReasoning(content ReasoningContent) {
	if content.Thinking == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !payloadsEnabledFrom(t.ctx) {
		return
	}
	t.Reasoning = append(t.Reasoning, content)
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
		// ending it. Both gen_ai.completion and gen_ai.output.messages are
		// emitted so backends that read either OTel-semconv variant of the
		// payload attribute pick it up. Gated on the WithPayloadsEnabled
		// ctx opt-in — same reasoning as prompt emission.
		if payloadsEnabledFrom(t.ctx) && err == nil {
			if payload, mErr := json.Marshal(result); mErr == nil && len(payload) > 0 && string(payload) != "null" {
				payloadStr, truncated := truncatePayload(string(payload))
				span.SetAttributes(
					attribute.String("gen_ai.completion", payloadStr),
					attribute.String("gen_ai.output.messages", payloadStr),
				)
				if truncated {
					span.SetAttributes(attribute.Bool("driftlessaf.payload.truncated", true))
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
		ID          string             `json:"id"`
		OTelTraceID string             `json:"otel_trace_id,omitempty"`
		InputPrompt string             `json:"input_prompt"`
		ExecContext ExecutionContext   `json:"exec_context,omitempty"`
		ToolCalls   []jsonToolCall     `json:"tool_calls"`
		Turns       []RecordedTurn     `json:"turns,omitempty"`
		Reasoning   []ReasoningContent `json:"reasoning,omitempty"`
		Result      T                  `json:"result"`
		Error       string             `json:"error,omitempty"`
		StartTime   time.Time          `json:"start_time"`
		EndTime     time.Time          `json:"end_time"`
		Metadata    map[string]any     `json:"metadata,omitempty"`
		AgentName   string             `json:"agent_name,omitempty"`
		Source      string             `json:"source,omitempty"`
		Model       string             `json:"model,omitempty"`
	}{
		ID:          t.ID,
		OTelTraceID: t.OTelTraceID,
		InputPrompt: t.InputPrompt,
		ExecContext: t.ExecContext,
		ToolCalls:   toolCalls,
		Turns:       t.Turns,
		Reasoning:   t.Reasoning,
		Result:      t.Result,
		Error:       errStr,
		StartTime:   t.StartTime,
		EndTime:     t.EndTime,
		Metadata:    t.Metadata,
		AgentName:   t.AgentName,
		Source:      t.Source,
		Model:       t.Model,
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
