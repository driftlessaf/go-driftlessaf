/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// dynamicTraceKeys are JSON keys whose values are non-deterministic
// (generated IDs, wall-clock timestamps) and must be excluded from
// whole-value comparisons.
var dynamicTraceKeys = map[string]struct{}{
	"id": {}, "start_time": {}, "end_time": {},
}

// ignoreDynamic is a cmp option that drops dynamic keys from map comparisons
// so that test expectations only contain deterministic fields.
var ignoreDynamic = cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
	_, ok := dynamicTraceKeys[k]
	return ok
})

// marshalRoundTrip marshals a trace and unmarshals it into a map, stripping
// dynamic keys from the result for deterministic comparison.
func marshalRoundTrip(t *testing.T, trace *Trace[string]) map[string]any {
	t.Helper()
	b, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return got
}

// TestTraceMarshalJSON verifies that Trace[T].MarshalJSON produces the
// expected JSON for a variety of scenarios. Each case builds a trace,
// marshals it, and checks the result against a full expected value
// (minus dynamic fields like id and timestamps).
func TestTraceMarshalJSON(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T) *Trace[string]
		want   map[string]any // full expected value (dynamic keys ignored by cmp)
		absent []string       // keys that must NOT appear
	}{{
		name: "basic fields and per-turn token usage",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithExecutionContext(t.Context(), ExecutionContext{
				ReconcilerKey:  "pr:owner/repo/42",
				ReconcilerType: "pr",
				CommitSHA:      "abc123",
			})
			trace := tracer.NewTrace(ctx, "test prompt")
			turn := trace.BeginTurn(0, "google.vertex", "gemini-2.5-flash")
			turn.RecordTokens(1500, 300)
			turn.End()
			trace.complete("the result", nil)
			return trace
		},
		// Trace.Model is latched from the first turn's model so single-call
		// traces still expose the model at the trace level for queries like
		// "cost by model". Token totals live only in turns[].
		want: map[string]any{
			"input_prompt": "test prompt",
			"result":       "the result",
			"model":        "gemini-2.5-flash",
			"tool_calls":   []any{},
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
			"exec_context": map[string]any{
				"reconciler_key":  "pr:owner/repo/42",
				"reconciler_type": "pr",
				"commit_sha":      "abc123",
			},
		},
		// Trace-level token fields were dropped — turns[] is the source of truth.
		absent: []string{"error", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens"},
	}, {
		name: "error serializes as string",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			trace.complete("", errors.New("something broke"))
			return trace
		},
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "",
			"error":        "something broke",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
		},
	}, {
		name: "tool call error serializes as string",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			tc := trace.StartToolCall("tc1", "failing_tool", map[string]any{"key": "val"})
			tc.Complete(nil, errors.New("tool failed"))
			trace.complete("done", nil)
			return trace
		},
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"exec_context": map[string]any{},
			"tool_calls": []any{
				map[string]any{
					"id":     "tc1",
					"name":   "failing_tool",
					"params": map[string]any{"key": "val"},
					"error":  "tool failed",
				},
			},
		},
	}, {
		// A genuine failure (above) omits recoverable via omitempty; a
		// rejection with a corrective hint serializes recoverable=true so
		// BQ can separate designed correction loops from real tool failures.
		name: "recoverable rejection serializes flag",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			trace.RejectedToolCall("tc1", "submit_result", map[string]any{"key": "val"}, errors.New("parameter error"))
			trace.complete("done", nil)
			return trace
		},
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"exec_context": map[string]any{},
			"tool_calls": []any{
				map[string]any{
					"id":          "tc1",
					"name":        "submit_result",
					"params":      map[string]any{"key": "val"},
					"error":       "parameter error",
					"recoverable": true,
				},
			},
		},
	}, {
		name: "unexported fields excluded from JSON",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			trace.complete("done", nil)
			return trace
		},
		absent: []string{"tracer", "mu", "ctx", "span"},
	}, {
		name: "metadata round-trips",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			trace.Metadata["group"] = "group-123"
			trace.Metadata["custom"] = float64(42)
			trace.complete("done", nil)
			return trace
		},
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
			"metadata": map[string]any{
				"group":  "group-123",
				"custom": float64(42),
			},
		},
	}, {
		name: "agent_name from ctx default",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithDefaultAgentName(t.Context(), "materializer")
			trace := tracer.NewTrace(ctx, "prompt")
			trace.complete("done", nil)
			return trace
		},
		// agent_name must reach the payload whenever it would reach the
		// gen_ai.agent.name span attr — single source of truth in newTrace.
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
			"agent_name":   "materializer",
		},
	}, {
		name: "agent_name opt overrides ctx default",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithDefaultAgentName(t.Context(), "ctx-default")
			trace := tracer.NewTrace(ctx, "prompt", WithAgentName("opt-override"))
			trace.complete("done", nil)
			return trace
		},
		// Explicit opt wins over ctx default — this is what the CloudEvent
		// middleware relies on to inject a per-T agent name.
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
			"agent_name":   "opt-override",
		},
	}, {
		name: "single turn round-trip with agent identity",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt", WithAgentName("materializer"))
			trace.Source = "octo-identity"
			turn := trace.BeginTurn(0, "google.vertex", "gemini-2.5-flash")
			turn.RecordTokens(100, 25)
			turn.End()
			trace.complete("done", nil)
			return trace
		},
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
			"agent_name":   "materializer",
			"source":       "octo-identity",
			"model":        "gemini-2.5-flash",
			"turns": []any{
				map[string]any{
					"index":         float64(0),
					"model":         "gemini-2.5-flash",
					"system":        "google.vertex",
					"input_tokens":  float64(100),
					"output_tokens": float64(25),
					"failed":        false,
				},
			},
		},
	}, {
		name: "multi-turn round-trip preserves order and per-turn fields",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			t0 := trace.BeginTurn(0, "anthropic", "model-a")
			t0.RecordTokens(100, 50)
			t0.RecordError(errors.New("transient 429"))
			t0.End()
			t1 := trace.BeginTurn(1, "openai", "model-b")
			t1.RecordTokens(200, 75)
			t1.Fail(errors.New("retries exhausted: upstream 503"))
			t1.End()
			trace.complete("done", nil)
			return trace
		},
		// Two distinct shapes the events-list + terminal-status model expresses:
		// - turn 0: recovered from a transient — errors logged, failed=false
		// - turn 1: terminal failure — errors logged AND failed=true
		// A clean success would have errors absent (omitempty) but failed=false
		// explicitly: BQ analytics use `failed = FALSE` without three-valued
		// logic across NULLs.
		want: map[string]any{
			"input_prompt": "prompt",
			"result":       "done",
			"tool_calls":   []any{},
			"exec_context": map[string]any{},
			// Trace.Model is latched from the first turn; per-turn models still
			// vary in turns[] when an executor switches mid-trace.
			"model": "model-a",
			"turns": []any{
				map[string]any{
					"index":         float64(0),
					"model":         "model-a",
					"system":        "anthropic",
					"input_tokens":  float64(100),
					"output_tokens": float64(50),
					"errors":        []any{"transient 429"},
					"failed":        false,
				},
				map[string]any{
					"index":         float64(1),
					"model":         "model-b",
					"system":        "openai",
					"input_tokens":  float64(200),
					"output_tokens": float64(75),
					"errors":        []any{"retries exhausted: upstream 503"},
					"failed":        true,
				},
			},
		},
	}, {
		name: "noop provider omits otel_trace_id",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			trace := tracer.NewTrace(t.Context(), "prompt")
			trace.complete("done", nil)
			return trace
		},
		// With the noop tracer provider the trace ID is all zeros,
		// which is !HasTraceID(), so OTelTraceID should be empty/omitted.
		absent: []string{"otel_trace_id"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := marshalRoundTrip(t, tt.setup(t))

			// Dynamic fields: just verify presence.
			if _, ok := got["id"]; !ok {
				t.Error("missing required field: id")
			}
			if _, ok := got["start_time"]; !ok {
				t.Error("missing required field: start_time")
			}

			if tt.want != nil {
				if diff := cmp.Diff(tt.want, got, ignoreDynamic); diff != "" {
					t.Errorf("MarshalJSON mismatch (-want +got):\n%s", diff)
				}
			}

			for _, key := range tt.absent {
				if v, ok := got[key]; ok && v != "" {
					t.Errorf("field %q should be absent, got %v", key, v)
				}
			}
		})
	}
}

// TestTraceMarshalJSON_IDMatchesStruct verifies the JSON id matches the
// struct ID field, since the table tests above can't know it in advance.
func TestTraceMarshalJSON_IDMatchesStruct(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	trace := tracer.NewTrace(t.Context(), "prompt")
	trace.complete("done", nil)

	got := marshalRoundTrip(t, trace)

	if got, want := fmt.Sprint(got["id"]), trace.ID; got != want {
		t.Errorf("id: got %q, want %q", got, want)
	}
}
