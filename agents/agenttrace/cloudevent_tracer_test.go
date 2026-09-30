/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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
	wrapped := WithCloudEventEmission[string](
		inner,
		client,
		"test-reconciler",
		WithBoundedTracePayloads[string](),
	)

	ctx := WithExecutionContext(t.Context(), ExecutionContext{
		ReconcilerKey:  "pr:owner/repo/42",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
	})
	ctx = WithPayloadsEnabled(ctx, true)
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

func TestWithCloudEventEmission_BoundsRootTracePayloads(t *testing.T) {
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	largePayload := strings.Repeat("x", 11_000_000)
	largeResult := map[string]any{"content": largePayload}
	var recorded *Trace[map[string]any]
	inner := ByCode[map[string]any](func(trace *Trace[map[string]any]) { recorded = trace })
	wrapped := WithCloudEventEmission[map[string]any](
		inner,
		client,
		"test-reconciler",
		WithBoundedTracePayloads[map[string]any](),
	)

	ctx := WithPayloadsEnabled(t.Context(), true)
	ctx = WithTracer[map[string]any](ctx, wrapped)
	trace, done := StartTrace[map[string]any](ctx, largePayload)
	call := trace.StartToolCall("tc1", "read_logs", map[string]any{"query": largePayload})
	call.Complete(map[string]any{"content": largePayload}, nil)
	trace.AppendReasoning(ReasoningContent{Thinking: largePayload})
	turn := trace.BeginTurn(0, "google.vertex", "gemini-2.5-flash")
	turn.RecordTokens(1500, 300)
	turn.End()
	done(largeResult, nil)
	drainCE[map[string]any](wrapped)

	if recorded == nil {
		t.Fatal("inner tracer did not receive the completed trace")
	}
	if got, want := recorded.InputPrompt, largePayload; got != want {
		t.Errorf("inner trace input prompt length: got %d, want %d", len(got), len(want))
	}
	if diff := cmp.Diff(largeResult, recorded.Result); diff != "" {
		t.Errorf("inner trace result (-want +got):\n%s", diff)
	}

	if len(body) > maxTraceEventDataBytes {
		t.Fatalf("bounded root event is %d bytes, limit is %d bytes", len(body), maxTraceEventDataBytes)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, string(body))
	}

	for _, field := range []string{"input_prompt", "reasoning", "result"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("root event omits payload field %q", field)
		}
	}
	inputPrompt, ok := decoded["input_prompt"].(string)
	if !ok || inputPrompt == "" || len(inputPrompt) > maxTracePayloadFieldBytes {
		t.Errorf("input_prompt: got %T with length %d, want a non-empty string of at most %d bytes", decoded["input_prompt"], len(inputPrompt), maxTracePayloadFieldBytes)
	}
	if diff := cmp.Diff(map[string]any{}, decoded["result"]); diff != "" {
		t.Errorf("result (-want +got):\n%s", diff)
	}
	reasoning, ok := decoded["reasoning"].([]any)
	if !ok || len(reasoning) != 1 {
		t.Fatalf("reasoning: got %#v, want one item", decoded["reasoning"])
	}
	reasoningItem, ok := reasoning[0].(map[string]any)
	if !ok {
		t.Fatalf("reasoning[0]: got %#v, want an object", reasoning[0])
	}
	thinking, ok := reasoningItem["thinking"].(string)
	if !ok || thinking == "" || len(thinking) > maxTracePayloadFieldBytes {
		t.Errorf("reasoning[0].thinking: got %T with length %d, want a non-empty string of at most %d bytes", reasoningItem["thinking"], len(thinking), maxTracePayloadFieldBytes)
	}
	metadata, ok := decoded["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata: got %#v, want an object", decoded["metadata"])
	}
	if got, want := metadata[payloadTruncatedMetadataField], true; got != want {
		t.Errorf("metadata[%q]: got %#v, want %#v", payloadTruncatedMetadataField, got, want)
	}

	toolCalls, ok := decoded["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("tool_calls: got %#v, want one call", decoded["tool_calls"])
	}
	toolCall, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0]: got %#v, want an object", toolCalls[0])
	}
	if diff := cmp.Diff(map[string]any{
		"id":     "tc1",
		"name":   "read_logs",
		"params": map[string]any{},
		"result": map[string]any{},
	}, toolCall, ignoreDynamic); diff != "" {
		t.Errorf("tool call (-want +got):\n%s", diff)
	}

	wantTurns := []any{map[string]any{
		"index":         float64(0),
		"model":         "gemini-2.5-flash",
		"system":        "google.vertex",
		"input_tokens":  float64(1500),
		"output_tokens": float64(300),
		"failed":        false,
	}}
	if diff := cmp.Diff(wantTurns, decoded["turns"], ignoreDynamic); diff != "" {
		t.Errorf("turns (-want +got):\n%s", diff)
	}
}

func TestBoundTracePayloadsRejectsUnboundedStructure(t *testing.T) {
	largeStructure := strings.Repeat("x", maxTraceEventDataBytes)
	tests := []struct {
		name  string
		event map[string]any
		want  string
	}{
		{
			name: "no payload fields",
			event: map[string]any{
				"metadata": map[string]any{"large": largeStructure},
			},
			want: "has no payload fields to truncate",
		},
		{
			name: "structural data exceeds limit",
			event: map[string]any{
				"input_prompt": "payload",
				"metadata":     map[string]any{"large": largeStructure},
			},
			want: "trace structural data is",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}

			bounded, err := boundTracePayloads(raw)
			if err == nil {
				t.Fatalf("boundTracePayloads returned %d bytes, want an error", len(bounded))
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error: got %q, want text %q", err, test.want)
			}
		})
	}
}

func TestBoundTracePayloadsDividesRemainingSpace(t *testing.T) {
	largePayload := strings.Repeat("x", maxTracePayloadFieldBytes+1024)
	toolCalls := make([]map[string]any, 128)
	for i := range toolCalls {
		toolCalls[i] = map[string]any{
			"id":     fmt.Sprintf("call-%d", i),
			"name":   "read_logs",
			"result": largePayload,
		}
	}
	event := map[string]any{
		"input_prompt": largePayload,
		"result":       largePayload,
		"tool_calls":   toolCalls,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if len(raw) <= maxTraceEventDataBytes {
		t.Fatalf("test event is %d bytes, want more than %d", len(raw), maxTraceEventDataBytes)
	}

	minimum, payloadFields, err := encodeTraceWithPayloadLimit(raw, 0)
	if err != nil {
		t.Fatalf("encode minimum event: %v", err)
	}
	fieldLimit := (maxTraceEventDataBytes - len(minimum)) / payloadFields
	if fieldLimit >= maxTracePayloadFieldBytes {
		t.Fatalf("per-field limit is %d bytes, want less than the flat %d-byte cap", fieldLimit, maxTracePayloadFieldBytes)
	}

	bounded, err := boundTracePayloads(raw)
	if err != nil {
		t.Fatalf("boundTracePayloads: %v", err)
	}
	if len(bounded) > maxTraceEventDataBytes {
		t.Fatalf("bounded event is %d bytes, limit is %d", len(bounded), maxTraceEventDataBytes)
	}

	var decoded struct {
		InputPrompt string `json:"input_prompt"`
		ToolCalls   []struct {
			Result string `json:"result"`
		} `json:"tool_calls"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(bounded, &decoded); err != nil {
		t.Fatalf("unmarshal bounded event: %v", err)
	}
	if got := len(decoded.InputPrompt); got == 0 || got > fieldLimit {
		t.Errorf("input prompt length: got %d, want 1..%d", got, fieldLimit)
	}
	if got := len(decoded.ToolCalls[0].Result); got == 0 || got > fieldLimit {
		t.Errorf("tool result length: got %d, want 1..%d", got, fieldLimit)
	}
	if got, want := decoded.Metadata[payloadTruncatedMetadataField], true; got != want {
		t.Errorf("metadata[%q]: got %#v, want %#v", payloadTruncatedMetadataField, got, want)
	}
}

func TestLimitJSONValuePreservesJSONKind(t *testing.T) {
	tests := []struct {
		name  string
		value json.RawMessage
		want  any
	}{
		{
			name:  "string",
			value: json.RawMessage(`"` + strings.Repeat("x", 20) + `"`),
			want:  strings.Repeat("x", 8),
		},
		{
			name:  "object",
			value: json.RawMessage(`{"key":"` + strings.Repeat("x", 20) + `"}`),
			want:  map[string]any{},
		},
		{
			name:  "array",
			value: json.RawMessage(`["` + strings.Repeat("x", 20) + `"]`),
			want:  []any{},
		},
		{
			name:  "number",
			value: json.RawMessage(strings.Repeat("9", 20)),
			want:  float64(0),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got any
			if err := json.Unmarshal(limitJSONValue(test.value, 10), &got); err != nil {
				t.Fatalf("unmarshal limited value: %v", err)
			}

			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("limited value (-want +got):\n%s", diff)
			}
		})
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

	ctx := WithPayloadsEnabled(t.Context(), true)
	ctx = WithTracer[string](ctx, wrapped)
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

var errSensitiveMarshal = errors.New("sensitive prompt from marshal error")

type marshalErrorResult struct{}

func (marshalErrorResult) MarshalJSON() ([]byte, error) {
	return nil, errSensitiveMarshal
}

func TestSetEventData_MarshalError(t *testing.T) {
	type errorIdentity struct {
		Sanitised bool
		Sensitive bool
	}

	tests := []struct {
		name            string
		payloadsEnabled bool
		want            errorIdentity
	}{
		{
			name: "payloads disabled",
			want: errorIdentity{
				Sanitised: true,
			},
		},
		{
			name:            "payloads enabled",
			payloadsEnabled: true,
			want: errorIdentity{
				Sensitive: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := WithPayloadsEnabled(t.Context(), test.payloadsEnabled)
			trace := &Trace[marshalErrorResult]{
				Result: marshalErrorResult{},
			}
			ce := cloudevents.NewEvent()
			tracer := &ceEmittingTracer[marshalErrorResult]{}

			err := tracer.setEventData(
				ctx,
				&ce,
				trace,
				omitSensitiveTraceFields,
				nil,
				sealSensitiveTraceFields,
			)
			got := errorIdentity{
				Sanitised: errors.Is(err, errTraceMarshal),
				Sensitive: errors.Is(err, errSensitiveMarshal),
			}

			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("marshal error identity (-want +got):\n%s", diff)
			}
		})
	}
}

// A trace built under WithPayloadsEnabled(ctx, true) (mentat's forced-on
// local-capture case) must still omit payload fields from the emitted
// CloudEvent when WithEmitPayloads(ctx, false) overrides emission, and must
// include them when the override is true. This is the mentat repair/bootstrap
// scenario: local capture (judge evidence, resume seed) always runs, but the
// CloudEvent sent to the shared broker must track record_llm_payloads
// independently.
func TestWithCloudEventEmission_EmitPayloadsOverridesCapture(t *testing.T) {
	tests := []struct {
		name         string
		emitPayloads bool
		wantPrompt   bool
	}{
		{name: "capture on, emit off omits payload", emitPayloads: false, wantPrompt: false},
		{name: "capture on, emit on includes payload", emitPayloads: true, wantPrompt: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			// Payloads always on for capture, exactly as mentat's authoring
			// call sites force it, with the emission flag set independently
			// from the (would-be) record_llm_payloads value.
			ctx := WithPayloadsEnabled(t.Context(), true)
			ctx = WithEmitPayloads(ctx, test.emitPayloads)
			ctx = WithTracer[string](ctx, wrapped)
			_, done := StartTrace[string](ctx, "sensitive authoring prompt")
			done("sensitive result", nil)
			drainCE[string](wrapped)

			if body == nil {
				t.Fatal("no CloudEvent body received")
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("body is not valid JSON: %v\nbody: %s", err, string(body))
			}

			_, hasPrompt := decoded["input_prompt"]
			if hasPrompt != test.wantPrompt {
				t.Errorf("input_prompt present = %v, want %v; body: %s", hasPrompt, test.wantPrompt, string(body))
			}
		})
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

// When WithPayloadsEnabled is unset, no per-span events should be emitted and
// the completed trace event should contain structural fields only.
func TestWithCloudEventEmission_PayloadsDisabled(t *testing.T) {
	var eventTypes []string
	var traceBody []byte
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		mu.Lock()
		eventTypes = append(eventTypes, r.Header.Get("Ce-Type"))
		if r.Header.Get("Ce-Type") == EventType {
			traceBody = body
		}
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
	toolCall := trace.StartToolCall("call-1", "read_logs", map[string]any{
		"path":      "/private/build.log",
		"reasoning": "inspect the failed step",
	})
	toolCall.Complete(map[string]any{"contents": "secret build output"}, nil)
	trace.Reasoning = []ReasoningContent{{Thinking: "secret model reasoning"}}
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
	if diff := cmp.Diff([]string{EventType}, eventTypes); diff != "" {
		t.Errorf("emitted event types (-want +got):\n%s", diff)
	}

	var decoded map[string]any
	if err := json.Unmarshal(traceBody, &decoded); err != nil {
		t.Fatalf("trace body is not valid JSON: %v\nbody: %s", err, string(traceBody))
	}
	want := map[string]any{
		"exec_context": map[string]any{},
		"model":        "claude-sonnet-4-7",
		"tool_calls": []any{
			map[string]any{
				"id":   "call-1",
				"name": "read_logs",
			},
		},
		"turns": []any{
			map[string]any{
				"index":  float64(0),
				"model":  "claude-sonnet-4-7",
				"system": "anthropic",
				"failed": false,
			},
		},
	}
	if diff := cmp.Diff(want, decoded, ignoreDynamic); diff != "" {
		t.Errorf("trace body with payloads disabled (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff(map[string]any{
		"path":      "/private/build.log",
		"reasoning": "inspect the failed step",
	}, trace.ToolCalls[0].Params); diff != "" {
		t.Errorf("in-memory tool-call params (-want +got):\n%s", diff)
	}
	if got, want := trace.Result, "done"; got != want {
		t.Errorf("in-memory result: got %q, want %q", got, want)
	}
}

var errRoundTrip = errors.New("round trip failed")

// errRoundTripper fails every request before it reaches the network. The
// CloudEvents client retries and then reports the send as a NACK.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errRoundTrip
}

type emissionKey struct {
	EventType string
	Outcome   string
}

// installMetricReader makes a fresh manual reader the global meter provider
// for the rest of the test. Emitters constructed afterwards record to it.
func installMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	return reader
}

// emissionCounts reads every agenttrace.cloudevent.emissions data point from
// reader, keyed by its event type and outcome attributes.
func emissionCounts(t *testing.T, reader *sdkmetric.ManualReader) map[emissionKey]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	counts := map[emissionKey]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "agenttrace.cloudevent.emissions" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is %T, want an int64 sum", md.Name, md.Data)
			}
			for _, dp := range sum.DataPoints {
				eventType, _ := dp.Attributes.Value("event_type")
				outcome, _ := dp.Attributes.Value("outcome")
				counts[emissionKey{eventType.AsString(), outcome.AsString()}] = dp.Value
			}
		}
	}

	return counts
}

// The emission counter is the only signal dashboards have for events that never
// reached the broker. Each event must be counted once, under its own event type
// and outcome, including an event dropped before a send is attempted.
func TestCloudEventEmissionOutcomes(t *testing.T) {
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okServer.Close()
	nackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer nackServer.Close()

	tests := []struct {
		name            string
		clientOpts      []cehttp.Option
		tracerOpts      []CEOption[string]
		payloadsEnabled bool
		record          func(t *testing.T, trace *Trace[string])
		want            map[emissionKey]int64
	}{
		{
			name:            "delivered",
			clientOpts:      []cehttp.Option{cloudevents.WithTarget(okServer.URL)},
			payloadsEnabled: true,
			record: func(t *testing.T, trace *Trace[string]) {
				turn := trace.BeginTurn(0, "google.vertex", "gemini-2.5-flash")
				if err := turn.RecordRequest([]map[string]string{{"role": "user", "content": "hi"}}); err != nil {
					t.Fatalf("RecordRequest: %v", err)
				}
				turn.End()
			},
			want: map[emissionKey]int64{
				{EventType, "delivered"}:     1,
				{SpanEventType, "delivered"}: 1,
			},
		},
		{
			name:       "server error",
			clientOpts: []cehttp.Option{cloudevents.WithTarget(nackServer.URL)},
			want:       map[emissionKey]int64{{EventType, "nack"}: 1},
		},
		{
			// The retries must not inflate the count: one event, one outcome.
			name: "transport failure",
			clientOpts: []cehttp.Option{
				cloudevents.WithTarget(okServer.URL),
				cehttp.WithRoundTripper(errRoundTripper{}),
			},
			want: map[emissionKey]int64{{EventType, "nack"}: 1},
		},
		{
			name:       "encoding failed",
			clientOpts: []cehttp.Option{cloudevents.WithTarget(okServer.URL)},
			tracerOpts: []CEOption[string]{WithBoundedTracePayloads[string]()},
			record: func(_ *testing.T, trace *Trace[string]) {
				trace.Metadata["large"] = strings.Repeat("x", maxTraceEventDataBytes)
			},
			want: map[emissionKey]int64{{EventType, "encoding_failed"}: 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := installMetricReader(t)

			client, err := cloudevents.NewClientHTTP(test.clientOpts...)
			if err != nil {
				t.Fatalf("creating test CE client: %v", err)
			}

			inner := ByCode[string](func(*Trace[string]) {})
			wrapped := WithCloudEventEmission[string](inner, client, "test-reconciler", test.tracerOpts...)

			ctx := WithPayloadsEnabled(t.Context(), test.payloadsEnabled)
			ctx = WithTracer[string](ctx, wrapped)

			trace, done := StartTrace[string](ctx, "prompt")
			if test.record != nil {
				test.record(t, trace)
			}
			done("result", nil)
			drainCE[string](wrapped)

			if diff := cmp.Diff(test.want, emissionCounts(t, reader)); diff != "" {
				t.Errorf("emission counts (-want +got):\n%s", diff)
			}
		})
	}
}
