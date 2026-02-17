/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

func TestToolClaudeMetadata(t *testing.T) {
	tool := Tool[string]{
		Def: Definition{
			Name:        "test_tool",
			Description: "A test tool",
			Parameters: []Parameter{
				{Name: "input", Type: "string", Description: "The input", Required: true},
				{Name: "count", Type: "integer", Description: "A count", Required: false},
			},
		},
		Handler: func(_ context.Context, call ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			return map[string]any{"received": call.Args["input"]}
		},
	}

	meta := tool.ClaudeMetadata()

	if meta.Definition.Name != "test_tool" {
		t.Errorf("got name %q, want %q", meta.Definition.Name, "test_tool")
	}

	props, ok := meta.Definition.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatal("properties is not map[string]any")
	}
	if len(props) != 2 {
		t.Errorf("got %d properties, want 2", len(props))
	}

	required := meta.Definition.InputSchema.Required
	if len(required) != 1 || required[0] != "input" {
		t.Errorf("got required %v, want [input]", required)
	}
}

func TestToolGoogleMetadata(t *testing.T) {
	tool := Tool[string]{
		Def: Definition{
			Name:        "test_tool",
			Description: "A test tool",
			Parameters: []Parameter{
				{Name: "input", Type: "string", Description: "The input", Required: true},
			},
		},
		Handler: func(_ context.Context, _ ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			return nil
		},
	}

	meta := tool.GoogleMetadata()

	if meta.Definition.Name != "test_tool" {
		t.Errorf("got name %q, want %q", meta.Definition.Name, "test_tool")
	}

	if len(meta.Definition.Parameters.Properties) != 1 {
		t.Errorf("got %d properties, want 1", len(meta.Definition.Parameters.Properties))
	}
}

func TestToolParam(t *testing.T) {
	ctx := context.Background()
	trace := agenttrace.StartTrace[string](ctx, "test")

	call := ToolCall{
		ID:   "test-1",
		Name: "test_tool",
		Args: map[string]any{"name": "hello", "count": float64(42)},
	}

	t.Run("string param", func(t *testing.T) {
		v, errResp := Param[string](call, trace, "name")
		if errResp != nil {
			t.Fatalf("unexpected error: %v", errResp)
		}
		if v != "hello" {
			t.Errorf("got %q, want %q", v, "hello")
		}
	})

	t.Run("int param from float64", func(t *testing.T) {
		v, errResp := Param[int](call, trace, "count")
		if errResp != nil {
			t.Fatalf("unexpected error: %v", errResp)
		}
		if v != 42 {
			t.Errorf("got %d, want 42", v)
		}
	})

	t.Run("missing param", func(t *testing.T) {
		_, errResp := Param[string](call, trace, "missing")
		if errResp == nil {
			t.Fatal("expected error for missing parameter")
		}
	})
}

func TestToolOptionalParam(t *testing.T) {
	call := ToolCall{
		ID:   "test-1",
		Name: "test_tool",
		Args: map[string]any{"name": "hello"},
	}

	t.Run("present", func(t *testing.T) {
		v, errResp := OptionalParam[string](call, "name", "default")
		if errResp != nil {
			t.Fatalf("unexpected error: %v", errResp)
		}
		if v != "hello" {
			t.Errorf("got %q, want %q", v, "hello")
		}
	})

	t.Run("missing uses default", func(t *testing.T) {
		v, errResp := OptionalParam[string](call, "missing", "default")
		if errResp != nil {
			t.Fatalf("unexpected error: %v", errResp)
		}
		if v != "default" {
			t.Errorf("got %q, want %q", v, "default")
		}
	})
}

func TestClaudeHandlerParsesJSON(t *testing.T) {
	tool := Tool[string]{
		Def: Definition{
			Name:        "echo",
			Description: "Echoes input",
			Parameters: []Parameter{
				{Name: "msg", Type: "string", Description: "Message", Required: true},
			},
		},
		Handler: func(_ context.Context, call ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			return map[string]any{"echo": call.Args["msg"]}
		},
	}

	meta := tool.ClaudeMetadata()
	ctx := context.Background()
	trace := agenttrace.StartTrace[string](ctx, "test")

	input, _ := json.Marshal(map[string]any{"msg": "hello"})
	toolUse := anthropic.ToolUseBlock{
		ID:    "t1",
		Name:  "echo",
		Input: input,
	}

	result := meta.Handler(ctx, toolUse, trace, nil)
	want := map[string]any{"echo": "hello"}
	if diff := cmp.Diff(want, result); diff != "" {
		t.Errorf("handler result mismatch (-want +got):\n%s", diff)
	}
}

func TestGoogleHandlerNilResponse(t *testing.T) {
	tool := Tool[string]{
		Def: Definition{
			Name:        "noop",
			Description: "Does nothing",
			Parameters:  []Parameter{},
		},
		Handler: func(_ context.Context, _ ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			return nil
		},
	}

	meta := tool.GoogleMetadata()
	ctx := context.Background()
	trace := agenttrace.StartTrace[string](ctx, "test")

	call := &genai.FunctionCall{
		ID:   "call-1",
		Name: "noop",
		Args: nil,
	}

	resp := meta.Handler(ctx, call, trace, nil)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ID != "call-1" {
		t.Errorf("got ID %q, want %q", resp.ID, "call-1")
	}
}
