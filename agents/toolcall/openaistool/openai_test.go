/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaistool

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go"
)

func TestError(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		args     []any
		expected map[string]any
	}{{
		name:     "simple error message",
		format:   "simple error",
		args:     nil,
		expected: map[string]any{"error": "simple error"},
	}, {
		name:     "formatted error",
		format:   "error: %s (code %d)",
		args:     []any{"not found", 404},
		expected: map[string]any{"error": "error: not found (code 404)"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Error(tt.format, tt.args...)
			if diff := cmp.Diff(tt.expected, got); diff != "" {
				t.Errorf("Error() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestErrorWithContext(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		context  map[string]any
		expected map[string]any
	}{{
		name:     "error with nil context",
		err:      errors.New("test error"),
		context:  nil,
		expected: map[string]any{"error": "test error"},
	}, {
		name:    "error with context fields",
		err:     errors.New("file not found"),
		context: map[string]any{"filename": "test.txt"},
		expected: map[string]any{
			"error":    "file not found",
			"filename": "test.txt",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ErrorWithContext(tt.err, tt.context)
			if diff := cmp.Diff(tt.expected, got); diff != "" {
				t.Errorf("ErrorWithContext() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestFromTool(t *testing.T) {
	sentinel := fmt.Sprintf("test-%d", rand.Int63())
	tool := toolcall.Tool[string]{
		Def: toolcall.Definition{
			Name:        "my_tool",
			Description: "Does things",
			Parameters: []toolcall.Parameter{{
				Name:        "query",
				Type:        "string",
				Description: "The search query",
				Required:    true,
			}, {
				Name:        "limit",
				Type:        "integer",
				Description: "Max results",
				Required:    false,
			}},
		},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[string], result *string) map[string]any {
			*result = sentinel
			return map[string]any{"ok": true}
		},
	}

	meta := FromTool(tool)

	// Verify definition structure.
	if meta.Definition.Function.Name != "my_tool" {
		t.Errorf("name: got = %s, wanted = my_tool", meta.Definition.Function.Name)
	}

	params := meta.Definition.Function.Parameters
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties: got = missing or wrong type")
	}

	// Verify auto-injected reasoning parameter.
	if _, ok := props["reasoning"]; !ok {
		t.Error("reasoning parameter: got = missing, wanted = present")
	}

	// Verify user-defined parameters.
	if _, ok := props["query"]; !ok {
		t.Error("query parameter: got = missing, wanted = present")
	}
	if _, ok := props["limit"]; !ok {
		t.Error("limit parameter: got = missing, wanted = present")
	}

	// Verify required list includes reasoning and query but not limit.
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("required: got = missing or wrong type")
	}
	reqSet := make(map[string]struct{}, len(required))
	for _, r := range required {
		reqSet[r] = struct{}{}
	}
	if _, ok := reqSet["reasoning"]; !ok {
		t.Error("required reasoning: got = missing, wanted = present")
	}
	if _, ok := reqSet["query"]; !ok {
		t.Error("required query: got = missing, wanted = present")
	}
	if _, ok := reqSet["limit"]; ok {
		t.Error("required limit: got = present, wanted = missing")
	}
}

func TestMap(t *testing.T) {
	tools := map[string]toolcall.Tool[string]{
		"tool_a": {
			Def: toolcall.Definition{Name: "tool_a", Description: "Tool A"},
		},
		"tool_b": {
			Def: toolcall.Definition{Name: "tool_b", Description: "Tool B"},
		},
	}

	mapped := Map(tools)
	if len(mapped) != 2 {
		t.Errorf("Map() length: got = %d, wanted = 2", len(mapped))
	}
	if _, ok := mapped["tool_a"]; !ok {
		t.Error("tool_a: got = missing, wanted = present")
	}
	if _, ok := mapped["tool_b"]; !ok {
		t.Error("tool_b: got = missing, wanted = present")
	}
}

func TestHandler_ValidArgs(t *testing.T) {
	var captured toolcall.ToolCall
	tool := toolcall.Tool[string]{
		Def: toolcall.Definition{Name: "test_tool"},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			captured = call
			return map[string]any{"ok": true}
		},
	}

	meta := FromTool(tool)
	tc := openai.ChatCompletionMessageToolCall{}
	tc.ID = "call_123"
	tc.Function.Name = "test_tool"
	tc.Function.Arguments = `{"reasoning":"testing","query":"hello"}`

	trace := agenttrace.StartTrace[string](context.Background(), "test")
	var result string
	meta.Handler(context.Background(), tc, trace, &result)

	if captured.ID != "call_123" {
		t.Errorf("call ID: got = %s, wanted = call_123", captured.ID)
	}
	if captured.Name != "test_tool" {
		t.Errorf("call name: got = %s, wanted = test_tool", captured.Name)
	}
	if captured.Args["query"] != "hello" {
		t.Errorf("call args.query: got = %v, wanted = hello", captured.Args["query"])
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	tool := toolcall.Tool[string]{
		Def: toolcall.Definition{Name: "test_tool"},
		Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			t.Error("handler should not be called on invalid JSON")
			return nil
		},
	}

	meta := FromTool(tool)
	tc := openai.ChatCompletionMessageToolCall{}
	tc.ID = "call_bad"
	tc.Function.Name = "test_tool"
	tc.Function.Arguments = "not json"

	trace := agenttrace.StartTrace[string](context.Background(), "test")
	var result string
	got := meta.Handler(context.Background(), tc, trace, &result)

	if _, hasError := got["error"]; !hasError {
		t.Error("handler result: got = no error key, wanted = error response")
	}
}
