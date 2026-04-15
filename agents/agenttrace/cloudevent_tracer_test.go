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

	wrapped := WithCloudEventEmission[string](inner, client, EventType, "test-source")

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
	wrapped := WithCloudEventEmission[string](inner, client, EventType, "test-reconciler")

	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
	})
	ctx = WithTracer[string](ctx, wrapped)
	trace, done := StartTrace[string](ctx, "fix the title")

	tc := trace.StartToolCall("tc1", "update_title", map[string]any{"title": "feat: new"})
	tc.Complete("done", nil)

	trace.RecordTokenUsage("gemini-2.5-flash", 1500, 300)
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
		"input_prompt":  "fix the title",
		"result":        "fixed",
		"model":         "gemini-2.5-flash",
		"input_tokens":  float64(1500),
		"output_tokens": float64(300),
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
	wrapped := WithCloudEventEmission[string](inner, client, EventType, "test-source")

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
