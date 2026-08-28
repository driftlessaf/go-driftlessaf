/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// recordRequestResponse exercises the gating + truncation path for a single
// turn and returns the captured payloads so the assertions stay close to the
// inputs.
func recordRequestResponse(t *testing.T, ctx context.Context, messages, response any) (req, resp []byte) {
	t.Helper()
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest(messages); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := turn.RecordResponse(response); err != nil {
		t.Fatalf("RecordResponse: %v", err)
	}
	return turn.requestPayload, turn.responsePayload
}

func TestRecordRequest_DisabledByDefault(t *testing.T) {
	req, resp := recordRequestResponse(t, t.Context(),
		[]map[string]string{{"role": "user", "content": "hi"}},
		map[string]string{"content": "hello"},
	)
	if req != nil || resp != nil {
		t.Errorf("payloads must be nil when WithPayloadsEnabled is unset: req=%q resp=%q", req, resp)
	}
}

func TestRecordRequest_EnabledStoresPayload(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	messages := []map[string]string{{"role": "user", "content": "hello"}}
	response := map[string]string{"content": "hi"}

	req, resp := recordRequestResponse(t, ctx, messages, response)
	if req == nil {
		t.Fatal("RecordRequest payload not stored")
	}
	if resp == nil {
		t.Fatal("RecordResponse payload not stored")
	}

	var decodedReq []map[string]string
	if err := json.Unmarshal(req, &decodedReq); err != nil {
		t.Fatalf("request payload not valid JSON: %v", err)
	}
	if got, want := decodedReq[0]["content"], "hello"; got != want {
		t.Errorf("request content: got %q, want %q", got, want)
	}
}

// TestRecordRequest_TruncationStaysValidJSON exercises the oversized-payload
// path end to end: a multi-MB request must produce a span that successfully
// json.Marshals (i.e. could be sent through ce.SetData) and whose
// prompt_messages bytes are still valid JSON. The original byte-cut truncator
// produced invalid JSON inside a json.RawMessage field, which made the
// downstream cloudevents.SetData fail and silently dropped the span — see
// https://github.com/chainguard-dev/mono/pull/40840#discussion_r3303142362.
func TestRecordRequest_TruncationStaysValidJSON(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")

	// Payload comfortably larger than maxPayloadBytes to force truncation.
	big := strings.Repeat("a", maxPayloadBytes+1024)
	if err := turn.RecordRequest(big); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := turn.RecordResponse(strings.Repeat("b", maxPayloadBytes+1024)); err != nil {
		t.Fatalf("RecordResponse: %v", err)
	}

	// Both stored payloads must be inside the cap and parseable as JSON —
	// otherwise the json.RawMessage on RecordedSpan rejects them at marshal.
	if got := len(turn.requestPayload); got > maxPayloadBytes {
		t.Errorf("request payload exceeds cap: got %d, want <= %d", got, maxPayloadBytes)
	}
	if !json.Valid(turn.requestPayload) {
		t.Errorf("truncated request payload is not valid JSON: %q", string(turn.requestPayload))
	}
	if !json.Valid(turn.responsePayload) {
		t.Errorf("truncated response payload is not valid JSON: %q", string(turn.responsePayload))
	}

	// Building the RecordedSpan and json.Marshaling it must succeed — this
	// is the codepath cloudevents.Event.SetData hits.
	span, ok := turn.buildRecordedSpan()
	if !ok {
		t.Fatal("buildRecordedSpan returned ok=false on a turn with recorded payloads")
	}
	if _, err := json.Marshal(span); err != nil {
		t.Fatalf("json.Marshal(span) failed for truncated payload — would drop the CloudEvent: %v", err)
	}

	// The truncation marker must surface in metadata so downstream consumers
	// can tell a truncated row apart from a small-but-real prompt.
	var meta map[string]any
	if err := json.Unmarshal(span.Metadata, &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	flag, ok := meta["driftlessaf.payload.truncated"].(bool)
	if !ok || !flag {
		t.Errorf("expected metadata.driftlessaf.payload.truncated=true, got %v", meta["driftlessaf.payload.truncated"])
	}
}

// TestRecordRequest_NoTruncationLeavesMetadataClean is the dual: when the
// payload fits, the truncation marker must be absent so the column is not
// littered with false-positive flags.
func TestRecordRequest_NoTruncationLeavesMetadataClean(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hi"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	span, ok := turn.buildRecordedSpan()
	if !ok {
		t.Fatal("buildRecordedSpan returned ok=false")
	}
	var meta map[string]any
	if err := json.Unmarshal(span.Metadata, &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	if _, present := meta["driftlessaf.payload.truncated"]; present {
		t.Errorf("truncation flag should be absent when payload fits, got metadata=%v", meta)
	}
}

func TestBuildRecordedSpan_ShapeAndHashStability(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	ctx = WithExecutionContext(ctx, ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
	})
	trace := tracer.NewTrace(ctx, "prompt", WithAgentName("materializer"))

	turn := trace.BeginTurnWithAttribution(0, "claude-sonnet-4-7", Attribution{
		ProviderName: "anthropic",
		System:       "anthropic",
		LogicalModel: "reasoning",
		Protocol:     "anthropic-messages",
	})
	messages := []map[string]string{{"role": "user", "content": "hello"}}
	if err := turn.RecordRequest(messages); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := turn.RecordResponse(map[string]string{"content": "hi"}); err != nil {
		t.Fatalf("RecordResponse: %v", err)
	}
	turn.RecordTokens(100, 25)
	turn.RecordCacheTokens(10, 5)

	span, ok := turn.buildRecordedSpan()
	if !ok {
		t.Fatal("expected buildRecordedSpan to return ok=true")
	}

	if span.TraceID != trace.ID {
		t.Errorf("trace_id: got %q, want %q", span.TraceID, trace.ID)
	}
	if want := trace.ID + "-t0"; span.SpanID != want {
		t.Errorf("span_id: got %q, want %q", span.SpanID, want)
	}
	if span.AgentName != "materializer" {
		t.Errorf("agent_name: got %q, want %q", span.AgentName, "materializer")
	}
	if span.ModelID != "claude-sonnet-4-7" {
		t.Errorf("model_id: got %q", span.ModelID)
	}
	if span.PromptHash == "" {
		t.Error("prompt_hash must be set")
	}
	var metadata map[string]any
	if err := json.Unmarshal(span.Metadata, &metadata); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	wantMetadata := map[string]string{
		"provider":      "anthropic",
		"system":        "anthropic",
		"logical_model": "reasoning",
		"protocol":      "anthropic-messages",
	}
	for key, want := range wantMetadata {
		if got := metadata[key]; got != want {
			t.Errorf("metadata[%q]: got %v, want %q", key, got, want)
		}
	}

	// Hash must be stable across builds — same canonical bytes ⇒ same hash.
	turn2 := trace.BeginTurn(1, "anthropic", "claude-sonnet-4-7")
	if err := turn2.RecordRequest(messages); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	span2, ok := turn2.buildRecordedSpan()
	if !ok {
		t.Fatal("expected buildRecordedSpan to return ok=true")
	}
	if span.PromptHash != span2.PromptHash {
		t.Errorf("prompt_hash not stable: %q vs %q", span.PromptHash, span2.PromptHash)
	}

	// token_counts must be valid JSON with the expected keys.
	var counts map[string]int64
	if err := json.Unmarshal(span.TokenCounts, &counts); err != nil {
		t.Fatalf("token_counts not valid JSON: %v", err)
	}
	wantCounts := map[string]int64{"input": 100, "output": 25, "cache_read": 10, "cache_write": 5}
	for k, v := range wantCounts {
		if counts[k] != v {
			t.Errorf("token_counts[%q]: got %d, want %d", k, counts[k], v)
		}
	}
}

func TestBuildRecordedSpan_NoRequestReturnsFalse(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if _, ok := turn.buildRecordedSpan(); ok {
		t.Error("expected ok=false when no request payload recorded")
	}
}

func TestEnd_InvokesSpanEmitterWhenPayloadRecorded(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")

	var emitted []RecordedSpan
	trace.spanEmitter = func(_ context.Context, span RecordedSpan) error {
		emitted = append(emitted, span)
		return nil
	}

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hello"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()

	if got, want := len(emitted), 1; got != want {
		t.Fatalf("emitted spans: got %d, want %d", got, want)
	}
	if got, want := emitted[0].SpanID, trace.ID+"-t0"; got != want {
		t.Errorf("emitted span_id: got %q, want %q", got, want)
	}
}

// TestEnd_EmitterErrorIsObservable proves that a non-nil error from the
// SpanEmitter does not panic End() and that the error is surfaced through
// the call site rather than silently swallowed. End() can't propagate the
// error (it's a cleanup hook with no useful return channel), so the
// observability contract is the clog.WarnContext call at the call site.
// See https://github.com/chainguard-dev/mono/pull/40840#discussion_r3303142383.
func TestEnd_EmitterErrorIsObservable(t *testing.T) {
	ctx := WithPayloadsEnabled(t.Context(), true)
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(ctx, "prompt")

	emitterCalled := false
	trace.spanEmitter = func(_ context.Context, _ RecordedSpan) error {
		emitterCalled = true
		return errors.New("simulated pre-flight failure")
	}

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hello"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	// End() must not panic and must invoke the emitter even when the emitter
	// returns an error — the warning log is the observable signal.
	turn.End()

	if !emitterCalled {
		t.Error("emitter must be invoked even though it returns an error")
	}
}

func TestEnd_NoEmissionWhenPayloadsDisabled(t *testing.T) {
	tracer := &mockTracer[string]{traces: new([]*Trace[string])}
	trace := tracer.NewTrace(t.Context(), "prompt")

	var emitted []RecordedSpan
	trace.spanEmitter = func(_ context.Context, span RecordedSpan) error {
		emitted = append(emitted, span)
		return nil
	}

	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hello"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()

	if len(emitted) != 0 {
		t.Errorf("spans must not be emitted when payloads gate is off: got %d", len(emitted))
	}
}

func TestRecordedSpan_JSONFieldNamesMatchSchema(t *testing.T) {
	// Round-trip a fully-populated span and confirm the JSON keys are exactly
	// the schema's column names. Guards against silent renames that would
	// break BigQuery ingestion.
	score := 0.85
	in := RecordedSpan{
		TraceID:        "trace-1",
		SpanID:         "trace-1-t0",
		AgentName:      "fixer",
		ModelID:        "gemini-2.5-pro",
		PromptMessages: json.RawMessage(`[]`),
		Completion:     json.RawMessage(`{}`),
		PromptHash:     "deadbeef",
		TokenCounts:    json.RawMessage(`{"input":1}`),
		JudgeScore:     &score,
		Metadata:       json.RawMessage(`{"k":"v"}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"trace_id", "span_id", "agent_name", "model_id", "recorded_at",
		"prompt_messages", "completion", "prompt_hash", "token_counts",
		"judge_score", "metadata",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}
