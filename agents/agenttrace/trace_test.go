/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// randomString generates a random string for testing
func randomString() string {
	return fmt.Sprintf("test-%d", rand.Int63())
}

// Whole-value coverage of agent name stamping (ctx default and opt override)
// lives in TestTraceMarshalJSON's "agent_name from ctx default" and
// "agent_name opt overrides ctx default" cases, where it sits alongside the
// rest of the trace shape and a single regression in newTrace fails both.

func TestNewTrace(t *testing.T) {
	prompt := randomString()
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := t.Context()
	trace := tracer.NewTrace(ctx, prompt)

	if trace == nil {
		t.Fatal("got = nil, wanted = non-nil trace")
	}

	if trace.InputPrompt != prompt {
		t.Errorf("prompt: got = %q, wanted = %q", trace.InputPrompt, prompt)
	}

	if trace.ID == "" {
		t.Error("trace ID: got = empty string, wanted = non-empty")
	}

	if trace.StartTime.IsZero() {
		t.Error("start time: got = zero time, wanted = set time")
	}

	if len(trace.ToolCalls) != 0 {
		t.Errorf("tool calls length: got = %d, wanted = 0", len(trace.ToolCalls))
	}

	if trace.Metadata == nil {
		t.Error("metadata: got = nil, wanted = initialized map")
	}
}

func TestTraceStartToolCall(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := t.Context()
	trace := tracer.NewTrace(ctx, randomString())

	params := map[string]any{
		"param1": "value1",
		"param2": 42,
	}
	result := map[string]any{
		"status": "success",
	}

	// Start a tool call
	toolName := randomString()
	tc := trace.StartToolCall("tc1", toolName, params)
	if tc == nil {
		t.Fatal("StartToolCall should return a non-nil *ToolCall")
	}

	if tc.Name != toolName {
		t.Errorf("tool name: got = %q, wanted = %q", tc.Name, toolName)
	}

	// Tool call should not be added to trace yet
	if len(trace.ToolCalls) != 0 {
		t.Errorf("tool calls length: got = %d, wanted = 0 (before completion)", len(trace.ToolCalls))
	}

	// Complete the tool call
	tc.Complete(result, nil)

	// Now it should be added
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after completion: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	if recordedTC := trace.ToolCalls[0]; recordedTC.Name != toolName {
		t.Errorf("recorded tool name: got = %q, wanted = %q", recordedTC.Name, toolName)
	} else if recordedTC.Error != nil {
		t.Errorf("recorded tool error: got = %v, wanted = nil", recordedTC.Error)
	}

	// Test tool call with error
	err := errors.New("test error")
	tc2 := trace.StartToolCall("tc2", "error-tool", nil)
	tc2.Complete(nil, err)

	if len(trace.ToolCalls) != 2 {
		t.Fatalf("tool calls length after second completion: got = %d, wanted = 2", len(trace.ToolCalls))
	}

	if recordedTC2 := trace.ToolCalls[1]; !errors.Is(recordedTC2.Error, err) {
		t.Errorf("second tool call error: got = %v, wanted = %v", recordedTC2.Error, err)
	}
}

func TestToolCallDuration(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := t.Context()
	trace := tracer.NewTrace(ctx, randomString())

	tc := trace.StartToolCall("tc1", randomString(), nil)

	// Test duration before completion
	time.Sleep(10 * time.Millisecond)
	duration1 := tc.Duration()
	if duration1 == 0 {
		t.Error("incomplete tool call duration: got = 0, wanted = non-zero")
	}

	// Complete the tool call
	result := randomString()
	tc.Complete(result, nil)
	duration2 := tc.Duration()
	if duration2 == 0 {
		t.Error("completed tool call duration: got = 0, wanted = non-zero")
	}

	// Duration should be consistent after completion
	time.Sleep(10 * time.Millisecond)
	duration3 := tc.Duration()
	if duration2 != duration3 {
		t.Errorf("duration consistency: got = %v, wanted = %v", duration3, duration2)
	}
}

func TestTraceComplete(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := WithTracer[string](t.Context(), tracer)
	trace := tracer.NewTrace(ctx, randomString())

	// Sleep briefly to ensure EndTime is different from StartTime
	time.Sleep(10 * time.Millisecond)

	result := randomString()
	trace.complete(result, nil)

	if trace.Result != result {
		t.Errorf("trace result: got = %v, wanted = %v", trace.Result, result)
	}

	if trace.Error != nil {
		t.Errorf("trace error: got = %v, wanted = nil", trace.Error)
	}

	if trace.EndTime.IsZero() {
		t.Error("end time: got = zero time, wanted = set time")
	}

	if trace.Duration() == 0 {
		t.Error("trace duration: got = 0, wanted = non-zero")
	}
}

func TestTraceCompleteWithError(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := WithTracer[string](t.Context(), tracer)
	trace := tracer.NewTrace(ctx, randomString())

	err := errors.New("test error")
	trace.complete("", err)

	if !errors.Is(trace.Error, err) {
		t.Errorf("trace error: got = %v, wanted = %v", trace.Error, err)
	}

	if !trace.EndTime.IsZero() && trace.EndTime.Before(trace.StartTime) {
		t.Errorf("end time order: got = %v before start time %v, wanted = after", trace.EndTime, trace.StartTime)
	}
}

func TestTraceDuration(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := WithTracer[string](t.Context(), tracer)
	trace := tracer.NewTrace(ctx, randomString())

	// Test duration before completion
	time.Sleep(10 * time.Millisecond)
	duration1 := trace.Duration()
	if duration1 == 0 {
		t.Error("incomplete trace duration: got = 0, wanted = non-zero")
	}

	// Test duration after completion
	trace.complete(randomString(), nil)
	duration2 := trace.Duration()
	if duration2 == 0 {
		t.Error("completed trace duration: got = 0, wanted = non-zero")
	}

	// Duration should be consistent after completion
	time.Sleep(10 * time.Millisecond)
	duration3 := trace.Duration()
	if duration2 != duration3 {
		t.Errorf("duration consistency: got = %v, wanted = %v", duration3, duration2)
	}
}

func TestGenerateTraceID(t *testing.T) {
	// Test that trace IDs are unique
	ids := make(map[string]struct{}, 100)
	for range 100 {
		id := generateTraceID()
		if id == "" {
			t.Error("generated ID: got = empty string, wanted = non-empty")
		}
		if _, exists := ids[id]; exists {
			t.Errorf("duplicate ID: got = %s (already seen), wanted = unique", id)
		}
		ids[id] = struct{}{}
	}
}

func TestBadToolCall(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := t.Context()
	trace := tracer.NewTrace(ctx, randomString())

	// Test BadToolCall
	err := errors.New("invalid parameters")
	trace.BadToolCall("bad-tc-1", "bad-tool", map[string]any{
		"invalid": "params",
	}, err)

	// Check that the bad tool call was added
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after BadToolCall: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	badTC := trace.ToolCalls[0]
	if badTC.ID != "bad-tc-1" {
		t.Errorf("bad tool call ID: got = %q, wanted = %q", badTC.ID, "bad-tc-1")
	}
	if badTC.Name != "bad-tool" {
		t.Errorf("bad tool call name: got = %q, wanted = %q", badTC.Name, "bad-tool")
	}
	if !errors.Is(badTC.Error, err) {
		t.Errorf("bad tool call error: got = %v, wanted = %v", badTC.Error, err)
	}
	if badTC.Result != nil {
		t.Errorf("bad tool call result: got = %v, wanted = nil", badTC.Result)
	}
	if badTC.StartTime.IsZero() || badTC.EndTime.IsZero() {
		t.Errorf("bad tool call times: start = %v, end = %v, wanted = both non-zero", badTC.StartTime, badTC.EndTime)
	}
	// The times should be very close (within a millisecond)
	duration := badTC.EndTime.Sub(badTC.StartTime)
	if duration > time.Millisecond {
		t.Errorf("bad tool call duration: got = %v, wanted = < 1ms", duration)
	}
	// Kind separation: BadToolCall records genuine failures, never
	// recoverable rejections.
	if badTC.Recoverable {
		t.Errorf("bad tool call recoverable: got = true, wanted = false")
	}
}

func TestRejectedToolCall(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	err := errors.New("parameter error")
	trace.RejectedToolCall("rej-tc-1", "submit_result", map[string]any{
		"result": `{"summary":"stringified"}`,
	}, err)

	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after RejectedToolCall: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	tc := trace.ToolCalls[0]
	if !errors.Is(tc.Error, err) {
		t.Errorf("rejected tool call error: got = %v, wanted = %v", tc.Error, err)
	}
	if !tc.Recoverable {
		t.Errorf("rejected tool call recoverable: got = false, wanted = true")
	}
	if trace.Error != nil {
		t.Errorf("trace error after RejectedToolCall: got = %v, wanted = nil (rejection is not terminal)", trace.Error)
	}
}

func TestCompleteRejected(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	err := errors.New("result rejected: validation raised 1 finding(s)")
	tc := trace.StartToolCall("tc-1", "submit_result", map[string]any{"result": "..."})
	tc.CompleteRejected(err)

	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after CompleteRejected: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	got := trace.ToolCalls[0]
	if !errors.Is(got.Error, err) {
		t.Errorf("rejected tool call error: got = %v, wanted = %v", got.Error, err)
	}
	if !got.Recoverable {
		t.Errorf("rejected tool call recoverable: got = false, wanted = true")
	}
	if got.EndTime.IsZero() {
		t.Errorf("rejected tool call end time: got = zero, wanted = set")
	}
}

func TestLLMTurnBeginAndEnd(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	originalCtx := trace.ctx

	// BeginTurn should replace trace.ctx with the turn's context.
	turn := trace.BeginTurn(0, "test-system", "test-model")
	if turn == nil {
		t.Fatal("BeginTurn: got = nil, wanted = non-nil LLMTurn")
	}
	if trace.ctx == originalCtx {
		t.Error("BeginTurn: trace.ctx unchanged, wanted = new turn context")
	}

	// End should restore trace.ctx to the pre-turn context.
	turn.End()
	if trace.ctx != originalCtx {
		t.Error("End: trace.ctx not restored, wanted = original context")
	}
}

func TestLLMTurnEndIdempotent(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "test-system", "test-model")
	savedCtx := trace.ctx

	// First End restores context.
	turn.End()
	if trace.ctx == savedCtx {
		t.Error("End (first call): trace.ctx not restored to original")
	}

	// Begin a second turn to change ctx again, then call End twice on first turn.
	turn2 := trace.BeginTurn(1, "test-system", "test-model")
	afterTurn2Ctx := trace.ctx

	// Second call to the first turn's End must be a no-op — must not overwrite
	// ctx that turn2 set.
	turn.End()
	if trace.ctx != afterTurn2Ctx {
		t.Error("End (second call): overwrote ctx set by turn2, wanted = no-op")
	}

	turn2.End()
}

// Whole-value coverage of single-turn and multi-turn append (with model,
// system, tokens, and payloads) lives in TestTraceMarshalJSON's "single turn
// round-trip with agent identity" and "multi-turn round-trip" cases.

// ignoreTurnTimes drops the wall-clock fields from RecordedTurn diffs so
// behavioral tests can assert whole-value on trace.Turns without flake.
var ignoreTurnTimes = cmpopts.IgnoreFields(RecordedTurn{}, "StartTime", "EndTime")

// End must be idempotent for the turn append so double-End doesn't produce
// duplicate rows. Asserts on the whole resulting Turns slice — a regression
// that mis-mutates index, tokens, or anything else fails the same diff.
func TestLLMTurnEndAppendIdempotent(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.RecordTokens(10, 5)
	turn.End()
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", InputTokens: 10, OutputTokens: 5},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns after double End (-want +got):\n%s", diff)
	}
}

// A turn that ends without any RecordTokens call (e.g. the LLM returned an
// error before token counts were known) still needs a row so the trace shows
// the attempt and its timing — with zero token counts and no payloads.
func TestLLMTurnEndWithoutRecordTokens(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic"},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// RecordError appends to the chronological Errors list without marking the
// turn failed. This is the recovery shape: a transient error happened, the
// turn went on to succeed, and BQ should see both facts (Errors non-empty,
// Failed false).
func TestLLMTurnRecordErrorAppendsWithoutFailing(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.RecordError(errors.New("transient 503"))
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", Errors: []string{"transient 503"}},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// A nil err must be a no-op so callers can blindly route every err through
// RecordError without nil-checking at every call site, and so the recovery
// shape `RecordError(err); RecordError(nil)` doesn't leave a confusing empty
// string in the Errors list.
func TestLLMTurnRecordErrorNil(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.RecordError(nil)
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic"},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// All RecordError calls must be preserved in chronological order — that's the
// whole point of moving to a list. BQ analytics like "count turns that saw a
// 429 followed by a 503" depend on the order being faithful to what happened.
func TestLLMTurnRecordErrorAppendsInOrder(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.RecordError(errors.New("first"))
	turn.RecordError(errors.New("second"))
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", Errors: []string{"first", "second"}},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// Fail marks the terminal outcome and also appends the cause to Errors so
// the BQ row has both: a flag for "did this fail?" and the cause in the
// chronological list. Otherwise queries would have to UNION two columns to
// reconstruct what happened.
func TestLLMTurnFail(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.Fail(errors.New("retries exhausted"))
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", Errors: []string{"retries exhausted"}, Failed: true},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// Fail(nil) intentionally still marks the turn failed — calling Fail means
// "this turn ended in failure," and that signal must reach BQ even when the
// caller has no concrete error value to log. Errors stays empty because
// there's nothing to append. This is the asymmetry with RecordError(nil),
// which is a no-op: RecordError logs an event if there's one to log, while
// Fail records terminal status whether or not a cause is supplied.
//
// The executor wiring guards `if err != nil { Fail(err) }` precisely because
// of this — without the guard, successful turns (err=nil) would be marked
// failed.
func TestLLMTurnFailNilStillMarksFailed(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.Fail(nil)
	turn.End()

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", Failed: true},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// The recovery shape: a transient error was recorded, then the turn went on
// to succeed. RecordError must NOT set Failed (that's Fail's job), so the
// Errors list captures what the turn endured while Failed reflects the
// terminal outcome. This is the case the prior single-string model could not
// represent.
func TestLLMTurnRecoveryAfterError(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "anthropic", "model")
	turn.RecordError(errors.New("transient 503"))
	turn.End() // no Fail call: the turn recovered

	want := []RecordedTurn{
		{Index: 0, Model: "model", System: "anthropic", Errors: []string{"transient 503"}},
	}
	if diff := cmp.Diff(want, trace.Turns, ignoreTurnTimes); diff != "" {
		t.Errorf("trace.Turns (-want +got):\n%s", diff)
	}
}

// Fail must mark the turn span as Error so OTEL backends (Cloud Trace, Tempo,
// Jaeger, etc.) light up the failure — parity with ToolCall.Complete and
// Trace.complete which both set codes.Error on the wrapped span. RecordError,
// in contrast, must NOT set status (it represents recoverable events) — only
// the exception event lands on the span.
func TestLLMTurnFailSetsSpanStatusRecordErrorDoesNot(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	// Recovered turn: RecordError only — span status stays Unset.
	rec := trace.BeginTurn(0, "test-system", "model-recovered")
	rec.RecordError(errors.New("transient"))
	rec.End()

	// Failed turn: Fail sets Error status with the cause as description.
	bad := trace.BeginTurn(1, "test-system", "model-failed")
	bad.Fail(errors.New("boom"))
	bad.End()

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		spans[s.Name()] = s
	}

	recSpan := spans["chat model-recovered"]
	if recSpan == nil {
		t.Fatal("chat model-recovered span not found")
	}
	if got, want := recSpan.Status().Code, codes.Unset; got != want {
		t.Errorf("recovered turn span status: got = %v, want = %v (RecordError must not set status)", got, want)
	}
	var recHasException bool
	for _, ev := range recSpan.Events() {
		if ev.Name == "exception" {
			recHasException = true
		}
	}
	if !recHasException {
		t.Error("recovered turn span: missing exception event from RecordError")
	}

	badSpan := spans["chat model-failed"]
	if badSpan == nil {
		t.Fatal("chat model-failed span not found")
	}
	if got, want := badSpan.Status().Code, codes.Error; got != want {
		t.Errorf("failed turn span status: got = %v, want = %v", got, want)
	}
	if got, want := badSpan.Status().Description, "boom"; got != want {
		t.Errorf("failed turn span description: got = %q, want = %q", got, want)
	}
}

func TestLLMTurnRecordTokens(t *testing.T) {
	// Use a real TracerProvider with a SpanRecorder so BeginTurn creates
	// SDK spans whose attributes we can inspect after End().
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn := trace.BeginTurn(0, "test-system", "test-model")
	turn.RecordTokens(1000, 200)
	turn.End()

	spans := sr.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded")
	}

	// Find the "chat test-model" span (the turn span).
	var chatAttrs []attribute.KeyValue
	for _, s := range spans {
		if s.Name() == "chat test-model" {
			chatAttrs = s.Attributes()
			break
		}
	}
	if chatAttrs == nil {
		t.Fatal("chat test-model span not found")
	}

	assertInt64Attr(t, chatAttrs, "gen_ai.usage.input_tokens", 1000)
	assertInt64Attr(t, chatAttrs, "gen_ai.usage.output_tokens", 200)
}

func assertInt64Attr(t *testing.T, attrs []attribute.KeyValue, key string, want int64) {
	t.Helper()
	for _, a := range attrs {
		if string(a.Key) == key {
			if got := a.Value.AsInt64(); got != want {
				t.Errorf("%s: got = %d, want = %d", key, got, want)
			}
			return
		}
	}
	t.Errorf("%s: not found in span attributes", key)
}

// TestBeginTurnBeforeEnd documents that overlapping turns corrupt the span
// hierarchy. If turn1.End() is called after BeginTurn(1), it restores a
// stale context and subsequent tool calls are parented under the trace
// span instead of turn2.
func TestBeginTurnBeforeEnd(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), randomString())

	turn1 := trace.BeginTurn(0, "test-system", "test-model")
	afterTurn1Ctx := trace.ctx

	// Begin turn2 without ending turn1 — violates the contract.
	turn2 := trace.BeginTurn(1, "test-system", "test-model")
	afterTurn2Ctx := trace.ctx

	// turn2 should be a child of the turn1 context (not the trace root),
	// because turn1 was still active when turn2 started.
	if afterTurn2Ctx == afterTurn1Ctx {
		t.Error("BeginTurn(1): ctx unchanged from turn1, wanted = new context")
	}

	// Now ending turn1 restores its stale pre-turn context, clobbering
	// turn2's context. This is the broken behavior the contract warns about.
	turn1.End()
	if trace.ctx == afterTurn2Ctx {
		t.Error("turn1.End(): expected stale ctx restore to clobber turn2 context")
	}

	turn2.End()
}

func TestTraceStringEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		setupFn  func() *Trace[string]
		contains []string // strings that should be in the output
		excludes []string // strings that should NOT be in the output
	}{{
		name: "empty trace",
		setupFn: func() *Trace[string] {
			return (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "")
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"\"",
			"No tool calls",
			"Result: ",
		},
	}, {
		name: "trace with very long prompt",
		setupFn: func() *Trace[string] {
			longPrompt := strings.Repeat("a", 1000)
			return (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), longPrompt)
		},
		contains: []string{
			"=== Trace",
			strings.Repeat("a", 1000), // Should include full prompt
			"No tool calls",
		},
	}, {
		name: "trace with very long result",
		setupFn: func() *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithTracer[string](context.Background(), tracer)
			trace := tracer.NewTrace(ctx, "test")
			longResult := strings.Repeat("b", 1000)
			trace.complete(longResult, nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"test\"",
			"No tool calls",
			"Result: " + strings.Repeat("b", 497) + "...", // Should be truncated at 500 chars
		},
		excludes: []string{
			strings.Repeat("b", 1000), // Full result should not appear
		},
	}, {
		name: "trace with metadata",
		setupFn: func() *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithTracer[string](context.Background(), tracer)
			trace := tracer.NewTrace(ctx, "test with metadata")
			trace.Metadata["custom_key"] = "custom_value"
			trace.Metadata["number"] = 42
			trace.complete("done", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"test with metadata\"",
			"Metadata:",
			"custom_key: custom_value",
			"number: 42",
		},
	}, {
		name: "trace with tool calls having long results",
		setupFn: func() *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithTracer[string](context.Background(), tracer)
			trace := tracer.NewTrace(ctx, "test")
			tc := trace.StartToolCall("tc1", "test-tool", map[string]any{
				"param1": "value1",
				"param2": 123,
			})
			longResult := strings.Repeat("c", 300)
			tc.Complete(longResult, nil)
			trace.complete("final", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (1):",
			"[1] test-tool (ID: tc1)",
			"param1: value1",
			"param2: 123",
			"Result: " + strings.Repeat("c", 197) + "...", // Tool call result truncated at 200 chars
		},
		excludes: []string{
			strings.Repeat("c", 300), // Full tool call result should not appear
		},
	}, {
		name: "trace with failed tool calls",
		setupFn: func() *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithTracer[string](context.Background(), tracer)
			trace := tracer.NewTrace(ctx, "test")
			tc := trace.StartToolCall("tc1", "failing-tool", nil)
			tc.Complete(nil, errors.New("tool failed"))
			trace.complete("partial success", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (1):",
			"[1] failing-tool (ID: tc1)",
			"Error: tool failed",
			"Result: partial success",
		},
	}, {
		name: "trace with mixed tool call states",
		setupFn: func() *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithTracer[string](context.Background(), tracer)
			trace := tracer.NewTrace(ctx, "mixed test")

			// Successful tool call
			tc1 := trace.StartToolCall("tc1", "success-tool", map[string]any{
				"key": "value",
			})
			tc1.Complete("success result", nil)

			// Failed tool call
			tc2 := trace.StartToolCall("tc2", "fail-tool", nil)
			tc2.Complete(nil, errors.New("failed"))

			// Tool call with nil result (no error)
			tc3 := trace.StartToolCall("tc3", "nil-tool", nil)
			tc3.Complete(nil, nil)

			trace.complete("mixed results", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (3):",
			"[1] success-tool (ID: tc1)",
			"key: value",
			"Result: success result",
			"[2] fail-tool (ID: tc2)",
			"Error: failed",
			"[3] nil-tool (ID: tc3)",
			"Result: mixed results",
		},
	}, {
		name: "trace that never completes",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "incomplete")
			tc := trace.StartToolCall("tc1", "incomplete-tool", nil)
			// Don't complete the tool call or trace
			_ = tc
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"incomplete\"",
			"No tool calls",
			"Result: ",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := tt.setupFn()
			output := trace.String()

			// Check required strings are present
			for _, required := range tt.contains {
				if !strings.Contains(output, required) {
					t.Errorf("Expected output to contain %q, but it didn't.\nFull output:\n%s", required, output)
				}
			}

			// Check excluded strings are not present
			for _, excluded := range tt.excludes {
				if strings.Contains(output, excluded) {
					t.Errorf("Expected output to NOT contain %q, but it did.\nFull output:\n%s", excluded, output)
				}
			}

			// Basic structure checks
			if !strings.Contains(output, "=== Trace") {
				t.Error("Output should contain trace header")
			}
			if !strings.Contains(output, "Duration:") {
				t.Error("Output should contain duration information")
			}
		})
	}
}
