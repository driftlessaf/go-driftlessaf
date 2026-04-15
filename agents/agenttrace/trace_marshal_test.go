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
var dynamicTraceKeys = map[string]bool{
	"id": true, "start_time": true, "end_time": true,
}

// ignoreDynamic is a cmp option that drops dynamic keys from map comparisons
// so that test expectations only contain deterministic fields.
var ignoreDynamic = cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
	return dynamicTraceKeys[k]
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
		name: "basic fields and token usage",
		setup: func(t *testing.T) *Trace[string] {
			tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
			ctx := WithExecutionContext(t.Context(), ExecutionContext{
				ReconcilerKey:  "pr:owner/repo/42",
				ReconcilerType: "pr",
				CommitSHA:      "abc123",
			})
			trace := tracer.NewTrace(ctx, "test prompt")
			trace.RecordTokenUsage("gemini-2.5-flash", 1500, 300)
			trace.complete("the result", nil)
			return trace
		},
		want: map[string]any{
			"input_prompt":  "test prompt",
			"result":        "the result",
			"model":         "gemini-2.5-flash",
			"input_tokens":  float64(1500),
			"output_tokens": float64(300),
			"tool_calls":    []any{},
			"exec_context": map[string]any{
				"reconciler_key":  "pr:owner/repo/42",
				"reconciler_type": "pr",
				"commit_sha":      "abc123",
			},
		},
		absent: []string{"error"},
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
