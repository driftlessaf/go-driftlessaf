/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	noop "go.opentelemetry.io/otel/trace/noop"
)

// findSpan returns the first recorded span with the given name, or nil.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// findAttr returns the first attribute with the given key on the span, or nil.
// A pointer return lets callers distinguish "attribute missing" from "attribute
// present with zero value".
func findAttr(s sdktrace.ReadOnlySpan, key string) *attribute.KeyValue {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			kv := a
			return &kv
		}
	}
	return nil
}

// present marks attributes in a want map whose value isn't asserted — only
// presence. Pair it with anyValue in a cmp.Diff call.
type present struct{}

// attrsAsMap collapses a span's attributes into a key/value map for
// whole-value equality assertions via cmp.Diff.
func attrsAsMap(s sdktrace.ReadOnlySpan) map[string]any {
	attrs := s.Attributes()
	m := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}

// anyValue treats a want-side `present{}` marker as equal to any got-side
// value of the same key. Use it with cmp.Diff so callers that only care
// about presence (not the exact payload contents) don't have to assert
// twice.
var anyValue = cmp.FilterValues(
	func(x, _ any) bool { _, ok := x.(present); return ok },
	cmp.Ignore(),
)

// setupRecorder installs a SpanRecorder as the global TracerProvider for the
// duration of the test and returns it. Any span started via otel.Tracer(...)
// during the test lands in the recorder. On cleanup the global provider is
// reset to the noop implementation — matching the package default so other
// tests in this file (e.g. TestTraceMarshalJSON / "noop provider omits
// otel_trace_id") see a span context with no trace ID.
func setupRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return sr
}

// driveAgentTrace creates a root invoke_agent span, opens one turn with the
// given system + model, records token usage on the turn, and completes the
// trace with the supplied result. It mirrors the minimum surface an executor
// exercises. Callers that want payload emission on the root span pass a ctx
// wrapped with WithPayloadsEnabled; the default ctx leaves payloads off.
func driveAgentTrace[T any](t *testing.T, ctx context.Context, spanName, system, model string, result T, opts ...StartTraceOption) {
	t.Helper()
	ctx = WithExecutionContext(ctx, ExecutionContext{
		ReconcilerKey:  "pr:chainguard-dev/mono/38632",
		ReconcilerType: "pr",
	})
	trace, done := StartTrace[T](ctx, spanName, opts...)
	turn := trace.BeginTurn(0, system, model)
	turn.RecordTokens(1000, 200)
	turn.End()
	done(result, nil)
}

// TestPayloadsEnabled verifies that when WithPayloadsEnabled(ctx, true) is
// set, the root invoke_agent span carries both OTel-semconv variants of the
// payload attributes (gen_ai.prompt + gen_ai.input.messages,
// gen_ai.completion + gen_ai.output.messages) and the agent name / dynamic
// invocation label; and that the turn span carries gen_ai.system alongside
// gen_ai.request.model + per-call token usage. gen_ai.usage.* token attrs
// and tokens.* / model custom attrs MUST NOT appear on the root span —
// per OTel GenAI semconv they belong on the per-call "chat <model>" span,
// not the orchestration invoke_agent span. The absence falls out of the
// whole-map comparison for free.
func TestPayloadsEnabled(t *testing.T) {
	sr := setupRecorder(t)

	result := map[string]any{"summary": "ok", "failures": []string{}}
	driveAgentTrace(t, WithPayloadsEnabled(t.Context(), true), "analyze these logs", "google.vertex", "gemini-2.5-flash", result,
		WithAgentName("loganalyzer"),
		WithNameFn(func(ec ExecutionContext) string { return "autofix: " + ec.ReconcilerKey }),
	)

	spans := sr.Ended()
	root := findSpan(spans, "invoke_agent gemini-2.5-flash")
	if root == nil {
		t.Fatalf("root invoke_agent span not found; got %d spans", len(spans))
	}

	wantRoot := map[string]any{
		"agent.prompt":                 "analyze these logs",
		"gen_ai.operation.name":        "invoke_agent",
		"gen_ai.agent.name":            "loganalyzer",
		"driftlessaf.invocation.label": "autofix: pr:chainguard-dev/mono/38632",
		"reconciler_key":               "pr:chainguard-dev/mono/38632",
		"reconciler_type":              "pr",
		"gen_ai.prompt":                present{},
		"gen_ai.input.messages":        present{},
		"gen_ai.completion":            present{},
		"gen_ai.output.messages":       present{},
	}
	if diff := cmp.Diff(wantRoot, attrsAsMap(root), anyValue); diff != "" {
		t.Errorf("root attrs (-want +got):\n%s", diff)
	}

	turn := findSpan(spans, "chat gemini-2.5-flash")
	if turn == nil {
		t.Fatal("chat gemini-2.5-flash turn span not found")
	}
	wantTurn := map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.request.model":       "gemini-2.5-flash",
		"gen_ai.system":              "google.vertex",
		"driftlessaf.turn.index":     int64(0),
		"gen_ai.usage.input_tokens":  int64(1000),
		"gen_ai.usage.output_tokens": int64(200),
	}
	if diff := cmp.Diff(wantTurn, attrsAsMap(turn), anyValue); diff != "" {
		t.Errorf("turn attrs (-want +got):\n%s", diff)
	}
}

func TestTurnRouteAttribution(t *testing.T) {
	sr := setupRecorder(t)

	trace, done := StartTrace[string](t.Context(), "route-aware request")
	turn := trace.BeginTurnWithAttribution(0, "publishers/google/models/gemini-2.5-flash", Attribution{
		ProviderName: "gcp.vertex_ai",
		System:       SystemGoogleVertex,
		LogicalModel: "fast",
		Protocol:     "google-genai",
	})
	turn.End()
	done("done", nil)

	span := findSpan(sr.Ended(), "chat publishers/google/models/gemini-2.5-flash")
	if span == nil {
		t.Fatal("attributed chat turn span not found")
	}
	want := map[string]any{
		"gen_ai.operation.name":     "chat",
		"gen_ai.provider.name":      "gcp.vertex_ai",
		"gen_ai.request.model":      "publishers/google/models/gemini-2.5-flash",
		"gen_ai.system":             "google.vertex",
		"driftlessaf.model.logical": "fast",
		"driftlessaf.protocol":      "google-genai",
		"driftlessaf.turn.index":    int64(0),
	}
	if diff := cmp.Diff(want, attrsAsMap(span), anyValue); diff != "" {
		t.Errorf("turn attrs (-want +got):\n%s", diff)
	}
}

// TestPayloadsDisabled verifies that with no WithPayloadsEnabled opt-in on
// the ctx, the root span has no prompt/completion payload attributes —
// including agent.prompt, which carries the identical rendered prompt and
// must ride the same gate — only the agent name and the invocation label.
// As in TestPayloadsEnabled, no token usage attrs land on the root — token
// usage is per-call and belongs on the turn span. Absence of the payload
// keys and the token attrs falls out of the whole-map comparison.
func TestPayloadsDisabled(t *testing.T) {
	sr := setupRecorder(t)

	result := map[string]any{"summary": "ok"}
	driveAgentTrace(t, t.Context(), "analyze these logs", "google.vertex", "gemini-2.5-flash", result,
		WithAgentName("loganalyzer"),
		WithNameFn(func(ec ExecutionContext) string { return "autofix: " + ec.ReconcilerKey }),
	)

	spans := sr.Ended()
	root := findSpan(spans, "invoke_agent gemini-2.5-flash")
	if root == nil {
		t.Fatalf("root invoke_agent span not found; got %d spans", len(spans))
	}

	wantRoot := map[string]any{
		"gen_ai.operation.name":        "invoke_agent",
		"gen_ai.agent.name":            "loganalyzer",
		"driftlessaf.invocation.label": "autofix: pr:chainguard-dev/mono/38632",
		"reconciler_key":               "pr:chainguard-dev/mono/38632",
		"reconciler_type":              "pr",
	}
	if diff := cmp.Diff(wantRoot, attrsAsMap(root), anyValue); diff != "" {
		t.Errorf("root attrs (-want +got):\n%s", diff)
	}

	turn := findSpan(spans, "chat gemini-2.5-flash")
	if turn == nil {
		t.Fatal("chat gemini-2.5-flash turn span not found")
	}
	wantTurn := map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.request.model":       "gemini-2.5-flash",
		"gen_ai.system":              "google.vertex",
		"driftlessaf.turn.index":     int64(0),
		"gen_ai.usage.input_tokens":  int64(1000),
		"gen_ai.usage.output_tokens": int64(200),
	}
	if diff := cmp.Diff(wantTurn, attrsAsMap(turn), anyValue); diff != "" {
		t.Errorf("turn attrs (-want +got):\n%s", diff)
	}
}

func TestToolCallPayloadPolicy(t *testing.T) {
	tests := []struct {
		name            string
		payloadsEnabled bool
		want            map[string]any
	}{
		{
			name:            "enabled",
			payloadsEnabled: true,
			want: map[string]any{
				"gen_ai.operation.name":      "execute_tool",
				"tool.name":                  "read_logs",
				"tool.id":                    "call-1",
				"gen_ai.input.messages":      `{"path":"/private/build.log","reasoning":"inspect the failed step"}`,
				"driftlessaf.tool.reasoning": "inspect the failed step",
				"gen_ai.output.messages":     `{"contents":"secret build output"}`,
			},
		},
		{
			name: "disabled",
			want: map[string]any{
				"gen_ai.operation.name": "execute_tool",
				"tool.name":             "read_logs",
				"tool.id":               "call-1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sr := setupRecorder(t)
			ctx := WithPayloadsEnabled(t.Context(), test.payloadsEnabled)
			trace := newTrace[string](ctx, "secret prompt")
			toolCall := trace.StartToolCall("call-1", "read_logs", map[string]any{
				"path":      "/private/build.log",
				"reasoning": "inspect the failed step",
			})
			toolCall.Complete(map[string]any{"contents": "secret build output"}, nil)
			trace.complete("done", nil)

			span := findSpan(sr.Ended(), "execute_tool read_logs")
			if span == nil {
				t.Fatal("execute_tool read_logs span not found")
			}
			if diff := cmp.Diff(test.want, attrsAsMap(span), anyValue); diff != "" {
				t.Errorf("tool-call attrs (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTruncation verifies that a prompt larger than maxPayloadBytes is
// truncated before being emitted as gen_ai.prompt / gen_ai.input.messages,
// and that driftlessaf.payload.truncated=true is stamped to signal the
// truncation to the backend.
func TestTruncation(t *testing.T) {
	sr := setupRecorder(t)

	// Prompt comfortably larger than maxPayloadBytes. The JSON wrapper
	// adds ~20 bytes around the raw string so the attribute string length
	// will be bounded by maxPayloadBytes after truncation.
	bigPrompt := strings.Repeat("x", 2*maxPayloadBytes)
	driveAgentTrace(t, WithPayloadsEnabled(t.Context(), true), bigPrompt, "google.vertex", "gemini-2.5-flash", map[string]any{},
		WithAgentName("loganalyzer"),
	)

	spans := sr.Ended()
	root := findSpan(spans, "invoke_agent gemini-2.5-flash")
	if root == nil {
		t.Fatal("root invoke_agent span not found")
	}

	kv := findAttr(root, "gen_ai.input.messages")
	if kv == nil {
		t.Fatal("root: missing gen_ai.input.messages")
	}
	got := len(kv.Value.AsString())
	if got > maxPayloadBytes {
		t.Errorf("gen_ai.input.messages length = %d; want <= %d", got, maxPayloadBytes)
	}

	trunc := findAttr(root, "driftlessaf.payload.truncated")
	if trunc == nil || !trunc.Value.AsBool() {
		t.Error("root: driftlessaf.payload.truncated != true after truncation")
	}
}

// TestNameFnNil verifies that when no nameFn is supplied the
// driftlessaf.invocation.label attribute falls back to the agent name.
func TestNameFnNil(t *testing.T) {
	sr := setupRecorder(t)

	driveAgentTrace(t, t.Context(), "hello", "google.vertex", "gemini-2.5-flash", map[string]any{},
		WithAgentName("loganalyzer"),
	)

	spans := sr.Ended()
	root := findSpan(spans, "invoke_agent gemini-2.5-flash")
	if root == nil {
		t.Fatal("root invoke_agent span not found")
	}
	kv := findAttr(root, "driftlessaf.invocation.label")
	if kv == nil {
		t.Fatal("root: missing driftlessaf.invocation.label")
	}
	if got, want := kv.Value.AsString(), "loganalyzer"; got != want {
		t.Errorf("driftlessaf.invocation.label: got %q, want %q", got, want)
	}
}

// TestDefaultAgentNameFromContext verifies that WithDefaultAgentName set
// on the context feeds the agent name into StartTrace without the caller
// passing WithAgentName explicitly. This covers the reconciler-layer
// plumbing where the agent name is set once at the top of the chain.
func TestDefaultAgentNameFromContext(t *testing.T) {
	sr := setupRecorder(t)

	ctx := t.Context()
	ctx = WithExecutionContext(ctx, ExecutionContext{ReconcilerKey: "pr:foo/bar/1"})
	ctx = WithDefaultAgentName(ctx, "judge")
	ctx = WithDefaultNameFn(ctx, func(ec ExecutionContext) string { return "autofix: " + ec.ReconcilerKey })

	trace, done := StartTrace[map[string]any](ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-haiku-4-5")
	turn.End()
	done(map[string]any{}, nil)

	// BeginTurn renames the root span to "invoke_agent <model>" the first
	// time a turn is opened — that follows OTel GenAI semconv "{operation}
	// {model}" without forcing every caller to pass a model up front.
	spans := sr.Ended()
	root := findSpan(spans, "invoke_agent claude-haiku-4-5")
	if root == nil {
		t.Fatalf("root invoke_agent span not found; got %d spans", len(spans))
	}
	// No agent.prompt: prompt-bearing span attributes are gated behind the
	// payload opt-in (WithPayloadsEnabled), which this context does not set.
	wantRoot := map[string]any{
		"gen_ai.operation.name":        "invoke_agent",
		"gen_ai.agent.name":            "judge",
		"driftlessaf.invocation.label": "autofix: pr:foo/bar/1",
		"reconciler_key":               "pr:foo/bar/1",
	}
	if diff := cmp.Diff(wantRoot, attrsAsMap(root), anyValue); diff != "" {
		t.Errorf("root attrs (-want +got):\n%s", diff)
	}
}
