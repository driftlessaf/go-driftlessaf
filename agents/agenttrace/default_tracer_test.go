/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

var ignoreLogTime = cmpopts.IgnoreMapEntries(func(key string, _ any) bool {
	return key == "time"
})

func TestDefaultTracerPayloadPolicy(t *testing.T) {
	tests := []struct {
		name            string
		payloadsEnabled bool
		metadataOnly    bool
		wantTrace       map[string]any
	}{
		{
			name:            "default tracer with payloads enabled",
			payloadsEnabled: true,
			wantTrace: map[string]any{
				"agent_name":   "test-agent",
				"exec_context": map[string]any{},
				"input_prompt": "secret prompt",
				"model":        "test-model",
				"reasoning": []any{
					map[string]any{"thinking": "secret reasoning"},
				},
				"result": "secret result",
				"tool_calls": []any{
					map[string]any{
						"id":     "call-1",
						"name":   "read_logs",
						"params": map[string]any{"path": "secret path"},
						"result": "secret tool result",
					},
				},
				"turns": []any{
					map[string]any{
						"failed":        false,
						"index":         float64(0),
						"input_tokens":  float64(10),
						"model":         "test-model",
						"output_tokens": float64(5),
						"system":        "test-system",
					},
				},
			},
		},
		{
			name: "default tracer with payloads disabled",
			wantTrace: map[string]any{
				"agent_name":   "test-agent",
				"exec_context": map[string]any{},
				"model":        "test-model",
				"tool_calls": []any{
					map[string]any{
						"id":   "call-1",
						"name": "read_logs",
					},
				},
				"turns": []any{
					map[string]any{
						"failed":        false,
						"index":         float64(0),
						"input_tokens":  float64(10),
						"model":         "test-model",
						"output_tokens": float64(5),
						"system":        "test-system",
					},
				},
			},
		},
		{
			name:            "metadata tracer with payloads enabled",
			payloadsEnabled: true,
			metadataOnly:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := clog.New(slog.NewJSONHandler(&buf, nil))
			ctx := clog.WithLogger(t.Context(), logger)
			ctx = WithPayloadsEnabled(ctx, test.payloadsEnabled)

			trace := newTrace[string](ctx, "secret prompt", WithAgentName("test-agent"))
			toolCall := trace.StartToolCall("call-1", "read_logs", map[string]any{"path": "secret path"})
			toolCall.Complete("secret tool result", nil)
			turn := trace.BeginTurn(0, "test-system", "test-model")
			turn.RecordTokens(10, 5)
			turn.End()
			trace.Reasoning = []ReasoningContent{{Thinking: "secret reasoning"}}
			trace.complete("secret result", nil)

			tracer := NewDefaultTracer[string](ctx)
			if test.metadataOnly {
				tracer = NewMetadataTracer[string](ctx)
			}
			tracer.RecordTrace(trace)

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal log record: %v\nrecord: %s", err, buf.String())
			}

			want := map[string]any{
				"level":       "INFO",
				"msg":         "Agent trace completed",
				"trace_id":    trace.ID,
				"agent_name":  "test-agent",
				"model":       "test-model",
				"duration_ms": float64(trace.Duration().Milliseconds()),
				"tool_calls":  float64(1),
				"failed":      false,
			}
			if test.wantTrace != nil {
				want["trace"] = test.wantTrace
			}

			if diff := cmp.Diff(want, got, ignoreDynamic, ignoreLogTime); diff != "" {
				t.Errorf("log record (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultTracerProjectionFailureIsMetadataOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := clog.New(slog.NewJSONHandler(&buf, nil))
	ctx := clog.WithLogger(t.Context(), logger)

	trace := newTrace[string](ctx, "secret prompt", WithAgentName("test-agent"))
	toolCall := trace.StartToolCall("call-1", "broken_tool", map[string]any{
		"secret": "must not appear",
		"value":  make(chan string),
	})
	toolCall.Complete("secret result", nil)
	trace.complete("secret completion", nil)

	NewDefaultTracer[string](ctx).RecordTrace(trace)

	var records []map[string]any
	decoder := json.NewDecoder(&buf)
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		records = append(records, record)
	}
	want := []map[string]any{
		{
			"level":    "WARN",
			"msg":      "Agent trace projection failed",
			"trace_id": trace.ID,
		},
		{
			"level":       "INFO",
			"msg":         "Agent trace completed",
			"trace_id":    trace.ID,
			"agent_name":  "test-agent",
			"model":       "",
			"duration_ms": float64(trace.Duration().Milliseconds()),
			"tool_calls":  float64(1),
			"failed":      false,
		},
	}
	if diff := cmp.Diff(want, records, ignoreDynamic, ignoreLogTime); diff != "" {
		t.Errorf("log records (-want +got):\n%s", diff)
	}
}
