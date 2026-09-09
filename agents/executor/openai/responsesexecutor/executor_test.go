/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type request struct{}

func (request) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) { return p, nil }

type answer struct {
	Text string `json:"text" jsonschema:"minLength=1"`
}

func config(t *testing.T) Config[answer] {
	t.Helper()
	p, err := promptbuilder.NewPrompt("Synthetic task: inspect fixture, then submit.")
	if err != nil {
		t.Fatal(err)
	}
	return Config[answer]{Model: "fixture-model", Attribution: agenttrace.Attribution{ProviderName: "aws.bedrock", System: "aws.bedrock", LogicalModel: "gpt-5.6-sol", Protocol: "openai-responses"}, UserPrompt: p, MaxTurns: 5, MaxTokens: 123, Effort: effort.XHigh}
}

func call(id, name, args string) map[string]any {
	return map[string]any{"id": "fc_" + id, "type": "function_call", "call_id": id, "name": name, "arguments": args, "status": "completed"}
}
func submit(id, text string) map[string]any {
	b, _ := json.Marshal(map[string]any{"reasoning": "finished fixture", "result": map[string]any{"text": text}})
	return call(id, "submit_result", string(b))
}

// Emit real Responses SSE shapes through the SDK's HTTP decoder. The completed
// event carries the authoritative output, including native reasoning state.
func emit(w http.ResponseWriter, items ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"in_progress\"}}\n\n")
	for i, item := range items {
		if item["type"] == "function_call" {
			args := item["arguments"].(string)
			for _, delta := range []string{args[:len(args)/2], args[len(args)/2:]} {
				b, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": i, "delta": delta})
				_, _ = fmt.Fprintf(w, "event: response.function_call_arguments.delta\ndata: %s\n\n", b)
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_fixture", "status": "completed", "output": items, "usage": map[string]any{"input_tokens": 100, "output_tokens": 30, "total_tokens": 130, "input_tokens_details": map[string]any{"cached_tokens": 20}, "output_tokens_details": map[string]any{"reasoning_tokens": 10}},
	}})
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
}

func serve(t *testing.T, h http.HandlerFunc, cfg Config[answer]) Interface[request, answer] {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	e, err := New[request](responses.NewResponseService(option.WithBaseURL(s.URL), option.WithHTTPClient(s.Client())), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestNativeContinuation(t *testing.T) {
	t.Parallel()
	var requests []map[string]any
	e := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			emit(w, map[string]any{"id": "rs_fixture", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-state"}, call("read", "read_fixture", `{"reasoning":"inspect"}`))
			return
		}
		emit(w, submit("submit", "done"))
	}, config(t))
	var toolCalls int
	tools := map[string]toolcall.Tool[answer]{"read_fixture": {Def: toolcall.Definition{Name: "read_fixture", InputSchemaDefs: map[string]*toolcall.Schema{"Fixture": {Type: "string"}}, InputSchemaExtensions: map[string]any{"x-fixture": true}}, Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[answer], _ *answer) map[string]any {
		toolCalls++
		return map[string]any{"fixture": "safe"}
	}}}
	got, err := e.Execute(t.Context(), request{}, tools)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(answer{Text: "done"}, got); diff != "" {
		t.Fatal(diff)
	}
	if len(requests) != 2 || toolCalls != 1 {
		t.Fatalf("requests=%d tool calls=%d", len(requests), toolCalls)
	}
	for _, r := range requests {
		if r["store"] != false || r["stream"] != true || r["max_output_tokens"] != float64(123) {
			t.Errorf("incorrect request flags: %v", r)
		}
		for _, key := range []string{"messages", "previous_response_id", "temperature", "max_tokens"} {
			if _, ok := r[key]; ok {
				t.Errorf("unexpected %s", key)
			}
		}
		if diff := cmp.Diff(map[string]any{"effort": "high"}, r["reasoning"]); diff != "" {
			t.Error(diff)
		}
	}
	items := requests[1]["input"].([]any)
	if len(items) != 4 {
		t.Fatalf("continuation items=%d, want 4", len(items))
	}
	if items[1].(map[string]any)["encrypted_content"] != "opaque-state" {
		t.Error("reasoning state not preserved")
	}
	if items[3].(map[string]any)["call_id"] != "read" || items[3].(map[string]any)["type"] != "function_call_output" {
		t.Error("native tool output missing")
	}
	def := requests[0]["tools"].([]any)[1].(map[string]any)["parameters"].(map[string]any)
	if def["$defs"] == nil || def["x-fixture"] != true {
		t.Error("tool schema extensions dropped")
	}
}

func TestInvalidArgumentsAndRejectedSubmissionRecover(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{`{"unterminated":`, `[]`, `null`, `"string"`, `{}`} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			var n int
			e := serve(t, func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					emit(w, call("bad", "submit_result", bad))
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				items := body["input"].([]any)
				if items[len(items)-1].(map[string]any)["type"] != "function_call_output" {
					t.Error("missing correction output")
				}
				emit(w, submit("valid", "fixed"))
			}, config(t))
			got, err := e.Execute(t.Context(), request{}, nil)
			if err != nil || got.Text != "fixed" || n != 2 {
				t.Fatalf("got=%v err=%v requests=%d", got, err, n)
			}
		})
	}
}

func TestTerminalWaitsForTools(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			t.Parallel()
			var done atomic.Int32
			cfg := config(t)
			cfg.ToolCallConcurrency = concurrency
			cfg.ResultValidators = []callbacks.ResultValidator[answer]{func(context.Context, answer, string) ([]callbacks.Finding, error) {
				if done.Load() != 2 {
					return nil, errors.New("validator ran before tools joined")
				}
				return nil, nil
			}}
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				emit(w, submit("s", "done"), call("one", "work", `{}`), call("two", "work", `{}`))
			}, cfg)
			tools := map[string]toolcall.Tool[answer]{"work": {Def: toolcall.Definition{Name: "work"}, Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[answer], result *answer) map[string]any {
				result.Text = "must not become final"
				done.Add(1)
				return map[string]any{"ok": true}
			}}}
			got, err := e.Execute(t.Context(), request{}, tools)
			if err != nil || got.Text != "done" {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

func TestMalformedStreamsNeverExecuteTools(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		`{"type":"response.function_call_arguments.delta","delta":"{broken"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete"}}`,
		`{"type":"response.completed","response":{"id":"r","status":"in_progress"}}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
		`{"type":"response.unrecognized"}`,
		`not-json`,
	} {
		t.Run(data, func(t *testing.T) {
			t.Parallel()
			n := 0
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				n++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			}, config(t))
			_, err := e.Execute(t.Context(), request{}, nil)
			if err == nil || n != 1 {
				t.Fatalf("err=%v requests=%d", err, n)
			}
		})
	}
}

func TestBoundedRecoveryAndDuplicateIDs(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"refusal", "unknown-tool", "invalid-json", "duplicate-id", "schema"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			n := 0
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				n++
				id := fmt.Sprint(n)
				switch kind {
				case "refusal":
					emit(w, map[string]any{"id": "m" + id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "refusal", "refusal": "cannot comply"}}})
				case "unknown-tool":
					emit(w, call(id, "unknown", `{}`))
				case "invalid-json":
					emit(w, call(id, "submit_result", `[]`))
				case "schema":
					emit(w, submit(id, ""))
				case "duplicate-id":
					emit(w, submit("same", "ok"), submit("same", "ok"))
				}
			}, config(t))
			_, err := e.Execute(t.Context(), request{}, nil)
			want := 3
			if kind == "duplicate-id" {
				want = 1
			}
			if err == nil || n != want {
				t.Fatalf("err=%v requests=%d, want %d", err, n, want)
			}
		})
	}
}

func TestHTTPRetryAndErrorRedaction(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			n := 0
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				n++
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"secret-canary","type":"invalid_request_error"}}`)
					return
				}
				emit(w, submit("s", "done"))
			}, config(t))
			_, err := e.Execute(t.Context(), request{}, nil)
			if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
				if err != nil || n != 2 {
					t.Fatalf("err=%v requests=%d", err, n)
				}
			} else if err == nil || n != 1 || strings.Contains(err.Error(), "secret-canary") {
				t.Fatalf("err=%v requests=%d", err, n)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	e := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}, config(t))
	_, err := e.Execute(ctx, request{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestBoundedBody(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, input, want string
		wantErr           bool
	}{
		{name: "below limit", input: "ab", want: "ab"},
		{name: "exact limit", input: "abc", want: "abc"},
		{name: "above limit", input: "abcd", want: "abc", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader(tt.input)), remaining: 3}
			b, err := io.ReadAll(r)
			if (err != nil) != tt.wantErr || string(b) != tt.want {
				t.Fatalf("body/error: got = %q/%v, want = %q/error %v", b, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestTurnMetricsOnLoopExit(t *testing.T) {
	// The global meter provider must not be changed by parallel tests.
	for _, tt := range []struct {
		name               string
		status, maxTurns   int
		accepted, canceled bool
		wantTurns          int64
		wantLimit          int64
	}{
		{name: "accepted", accepted: true, maxTurns: 5, wantTurns: 1},
		{name: "provider failure", status: http.StatusBadRequest, maxTurns: 5, wantTurns: 1},
		{name: "retry exhausted", status: http.StatusServiceUnavailable, maxTurns: 5, wantTurns: 1},
		{name: "unusable turns", maxTurns: 5, wantTurns: 3},
		{name: "maximum turns", maxTurns: 2, wantTurns: 2, wantLimit: 1},
		{name: "already canceled", canceled: true, maxTurns: 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			previous := otel.GetMeterProvider()
			otel.SetMeterProvider(provider)
			defer otel.SetMeterProvider(previous)
			defer func() {
				if err := provider.Shutdown(t.Context()); err != nil {
					t.Error(err)
				}
			}()
			cfg := config(t)
			cfg.MaxTurns = tt.maxTurns
			e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				switch {
				case tt.status != 0:
					http.Error(w, "fixture failure", tt.status)
				case tt.accepted:
					emit(w, submit("done", "complete"))
				default:
					emit(w)
				}
			}, cfg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			_, err := e.Execute(ctx, request{}, nil)
			if (err == nil) != tt.accepted {
				t.Fatalf("error: got = %v, want success = %v", err, tt.accepted)
			}
			var data metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &data); err != nil {
				t.Fatal(err)
			}
			var count uint64
			var turns, limit int64
			for _, scope := range data.ScopeMetrics {
				for _, metric := range scope.Metrics {
					switch metric.Name {
					case "genai.agent.turns":
						for _, point := range metric.Data.(metricdata.Histogram[int64]).DataPoints {
							count += point.Count
							turns += point.Sum
						}
					case "genai.agent.turn_limit_exceeded":
						for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
							limit += point.Value
						}
					}
				}
			}
			if count != 1 || turns != tt.wantTurns || limit != tt.wantLimit {
				t.Errorf("turn metrics: got = (%d, %d, %d), want = (1, %d, %d)", count, turns, limit, tt.wantTurns, tt.wantLimit)
			}
		})
	}
}

type recordingTracer struct{ trace *agenttrace.Trace[answer] }

func (r *recordingTracer) NewTrace(ctx context.Context, prompt string, opts ...agenttrace.StartTraceOption) *agenttrace.Trace[answer] {
	return agenttrace.NewDefaultTracer[answer](ctx).NewTrace(ctx, prompt, opts...)
}
func (r *recordingTracer) RecordTrace(trace *agenttrace.Trace[answer]) { r.trace = trace }

func TestTraceUsageAttributionAndOpaqueRedaction(t *testing.T) {
	t.Parallel()
	e := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		emit(w, map[string]any{"type": "reasoning", "id": "r", "summary": []any{}, "encrypted_content": "private-canary"}, submit("s", "done"))
	}, config(t))
	tracer := new(recordingTracer)
	_, err := e.Execute(agenttrace.WithTracer[answer](t.Context(), tracer), request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tracer.trace == nil || len(tracer.trace.Turns) != 1 {
		t.Fatal("missing turn trace")
	}
	turn := tracer.trace.Turns[0]
	if turn.InputTokens != 100 || turn.OutputTokens != 30 || turn.ReasoningTokens != 10 || turn.CacheReadTokens != 20 || turn.Protocol != "openai-responses" || turn.Provider != "aws.bedrock" {
		t.Fatalf("turn=%+v", turn)
	}
	payload, err := safePayload(map[string]any{"input": []any{map[string]any{"type": "reasoning", "encrypted_content": "private-canary"}}, "text": "visible"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-canary") || !strings.Contains(string(b), "visible") {
		t.Fatalf("payload=%s", b)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, edit := range []func(*Config[answer]){
		func(c *Config[answer]) { c.UserPrompt = nil }, func(c *Config[answer]) { c.Model = " " },
		func(c *Config[answer]) { c.Attribution.ProviderName = "" }, func(c *Config[answer]) { c.Attribution.Protocol = "openai-chat-completions" },
		func(c *Config[answer]) { c.MaxTurns = -1 }, func(c *Config[answer]) { c.MaxTokens = -1 }, func(c *Config[answer]) { c.ToolCallConcurrency = -1 },
		func(c *Config[answer]) { c.Effort = "invalid" }, func(c *Config[answer]) { c.ResultValidators = []callbacks.ResultValidator[answer]{nil} },
		func(c *Config[answer]) { c.Submit.OmitPayloadFields = []string{"unknown"} },
	} {
		cfg := config(t)
		edit(&cfg)
		if _, err := New[request](responses.ResponseService{}, cfg); err == nil {
			t.Error("invalid config accepted")
		}
	}
}

func TestValidatorFailureAbortsWithoutContinuation(t *testing.T) {
	t.Parallel()
	cfg := config(t)
	want := errors.New("fixture validator failed")
	cfg.ResultValidators = []callbacks.ResultValidator[answer]{func(context.Context, answer, string) ([]callbacks.Finding, error) { return nil, want }}
	n := 0
	e := serve(t, func(w http.ResponseWriter, _ *http.Request) { n++; emit(w, submit("s", "done")) }, cfg)
	_, err := e.Execute(t.Context(), request{}, nil)
	if !errors.Is(err, want) || n != 1 {
		t.Fatalf("err=%v requests=%d", err, n)
	}
}
