/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/google/go-cmp/cmp"
)

// drainCE type-asserts the tracer to access Drain, flushing in-flight sends.
func drainCE[T any](tracer Tracer[T]) {
	if d, ok := tracer.(*ceEmittingTracer[T]); ok {
		d.Drain()
	}
}

// The CE decorator must always delegate to the inner tracer's RecordTrace,
// so existing logging/eval hooks still fire when CE emission is layered on.
func TestWithCloudEventEmission_DelegatesToInner(t *testing.T) {
	var recorded []*Trace[string]
	inner := ByCode[string](func(trace *Trace[string]) {
		recorded = append(recorded, trace)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		t.Fatalf("creating test CE client: %v", err)
	}

	wrapped := WithCloudEventEmission[string](inner, client, "test-source")

	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
	})
	ctx = WithTracer[string](ctx, wrapped)
	_, done := StartTrace[string](ctx, "test prompt")
	done("result", nil)
	drainCE[string](wrapped)

	if got, want := len(recorded), 1; got != want {
		t.Errorf("inner RecordTrace calls: got %d, want %d", got, want)
	}
}

// A completed trace must be emitted as a CloudEvent with correct headers
// (type, source, subject) and the full JSON-serialized trace as the body,
// so downstream consumers (BigQuery ingestion) receive a complete record.
func TestWithCloudEventEmission_EmitsCloudEvent(t *testing.T) {
	var received *http.Request
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		t.Fatalf("creating test CE client: %v", err)
	}

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-reconciler")

	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
	})
	ctx = WithTracer[string](ctx, wrapped)
	trace, done := StartTrace[string](ctx, "fix the title")

	tc := trace.StartToolCall("tc1", "update_title", map[string]any{"title": "feat: new"})
	tc.Complete("done", nil)

	turn := trace.BeginTurn(0, "google.vertex", "gemini-2.5-flash")
	turn.RecordTokens(1500, 300)
	turn.End()
	done("fixed", nil)
	drainCE[string](wrapped)

	if received == nil {
		t.Fatal("no HTTP request received by test server")
	}

	// Verify CloudEvent headers.
	wantHeaders := map[string]string{
		"Ce-Type":    EventType,
		"Ce-Source":  "test-reconciler",
		"Ce-Subject": "pr:owner/repo/42",
	}
	for header, want := range wantHeaders {
		if got := received.Header.Get(header); got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}

	// Verify body contains expected trace fields.
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, string(body))
	}

	want := map[string]any{
		"input_prompt": "fix the title",
		"result":       "fixed",
		"model":        "gemini-2.5-flash",
		"exec_context": map[string]any{
			"reconciler_key":  "pr:owner/repo/42",
			"reconciler_type": "pr",
			"commit_sha":      "abc123",
		},
		"tool_calls": []any{
			map[string]any{
				"id":     "tc1",
				"name":   "update_title",
				"params": map[string]any{"title": "feat: new"},
				"result": "done",
			},
		},
		"turns": []any{
			map[string]any{
				"index":         float64(0),
				"model":         "gemini-2.5-flash",
				"system":        "google.vertex",
				"input_tokens":  float64(1500),
				"output_tokens": float64(300),
				"failed":        false,
			},
		},
	}
	if diff := cmp.Diff(want, decoded, ignoreDynamic); diff != "" {
		t.Errorf("CE body mismatch (-want +got):\n%s", diff)
	}
}

// Errors on the trace must serialize as strings in the CloudEvent body,
// since error is not natively JSON-serializable.
func TestWithCloudEventEmission_ErrorSerializesAsString(t *testing.T) {
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		t.Fatalf("creating test CE client: %v", err)
	}

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-source")

	ctx := WithTracer[string](t.Context(), wrapped)
	_, done := StartTrace[string](ctx, "prompt")
	done("", errors.New("something went wrong"))
	drainCE[string](wrapped)

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, string(body))
	}

	if diff := cmp.Diff(map[string]any{
		"input_prompt": "prompt",
		"result":       "",
		"error":        "something went wrong",
		"tool_calls":   []any{},
		"exec_context": map[string]any{},
	}, decoded, ignoreDynamic); diff != "" {
		t.Errorf("CE body mismatch (-want +got):\n%s", diff)
	}
}

func TestNewBrokerClient_EmptyURL_ReturnsNil(t *testing.T) {
	client := NewBrokerClient(t.Context(), "")
	if client != nil {
		t.Error("expected nil client for empty broker URL")
	}
}

func TestNewBrokerClientImpersonating_EmptyURL_ReturnsNil(t *testing.T) {
	client := NewBrokerClientImpersonating(t.Context(), "", "emit@example.iam.gserviceaccount.com")
	if client != nil {
		t.Error("expected nil client for empty broker URL")
	}
}

// A turn that records a payload while payloads are enabled must produce a
// distinct per-span CloudEvent (SpanEventType) on the same broker, in
// addition to the per-trace event emitted at trace completion.
func TestWithCloudEventEmission_EmitsPerSpanEvent(t *testing.T) {
	var received []*http.Request
	var bodies [][]byte
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, r)
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		t.Fatalf("creating test CE client: %v", err)
	}

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-reconciler")

	ctx := WithPayloadsEnabled(t.Context(), true)
	ctx = WithExecutionContext(ctx, ExecutionContext{
		ReconcilerKey: "pr:owner/repo/42",
	})
	ctx = WithTracer[string](ctx, wrapped)

	trace, done := StartTrace[string](ctx, "fix the title")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "fix it"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	if err := turn.RecordResponse(map[string]string{"content": "fixed"}); err != nil {
		t.Fatalf("RecordResponse: %v", err)
	}
	turn.RecordTokens(100, 25)
	turn.End()
	done("fixed", nil)
	drainCE[string](wrapped)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 CloudEvents (1 span + 1 trace), got %d", len(received))
	}

	// Find the span event by type header.
	var spanBody []byte
	for i, r := range received {
		if r.Header.Get("Ce-Type") == SpanEventType {
			spanBody = bodies[i]
			break
		}
	}
	if spanBody == nil {
		t.Fatal("no CloudEvent with span type received")
	}

	var decoded map[string]any
	if err := json.Unmarshal(spanBody, &decoded); err != nil {
		t.Fatalf("span body not valid JSON: %v", err)
	}
	if got, want := decoded["trace_id"], trace.ID; got != want {
		t.Errorf("trace_id: got %v, want %v", got, want)
	}
	if got, want := decoded["span_id"], trace.ID+"-t0"; got != want {
		t.Errorf("span_id: got %v, want %v", got, want)
	}
	if decoded["prompt_hash"] == "" || decoded["prompt_hash"] == nil {
		t.Error("prompt_hash missing")
	}
}

// When WithPayloadsEnabled is unset, no per-span events should be emitted —
// only the per-trace event at completion.
func TestWithCloudEventEmission_NoSpanEventWhenPayloadsDisabled(t *testing.T) {
	var typeCounts = map[string]int{}
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		typeCounts[r.Header.Get("Ce-Type")]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := cloudevents.NewClientHTTP(
		cloudevents.WithTarget(srv.URL),
		cehttp.WithClient(*srv.Client()),
	)
	if err != nil {
		t.Fatalf("creating test CE client: %v", err)
	}

	inner := ByCode[string](func(_ *Trace[string]) {})
	wrapped := WithCloudEventEmission[string](inner, client, "test-reconciler")

	ctx := WithTracer[string](t.Context(), wrapped)
	trace, done := StartTrace[string](ctx, "prompt")
	turn := trace.BeginTurn(0, "anthropic", "claude-sonnet-4-7")
	// RecordRequest is a no-op since payloads are not enabled.
	if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hi"}}); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}
	turn.End()
	done("done", nil)
	drainCE[string](wrapped)

	mu.Lock()
	defer mu.Unlock()
	if got := typeCounts[SpanEventType]; got != 0 {
		t.Errorf("span events with payloads disabled: got %d, want 0", got)
	}
	if got := typeCounts[EventType]; got != 1 {
		t.Errorf("trace events: got %d, want 1", got)
	}
}
