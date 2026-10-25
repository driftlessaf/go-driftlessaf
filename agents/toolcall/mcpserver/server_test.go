/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMcpTool(t *testing.T) {
	tests := []struct {
		name string
		def  toolcall.Definition
	}{{
		name: "no parameters",
		def: toolcall.Definition{
			Name:        "ping",
			Description: "Ping the server",
		},
	}, {
		name: "required and optional parameters",
		def: toolcall.Definition{
			Name:        "read_file",
			Description: "Read a file from disk",
			Parameters: []toolcall.Parameter{{
				Name:        "path",
				Type:        "string",
				Description: "File path",
				Required:    true,
			}, {
				Name:        "offset",
				Type:        "integer",
				Description: "Line offset",
				Required:    false,
			}},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mcpTool(tt.def)
			if got.Name != tt.def.Name {
				t.Errorf("name: got = %q, wanted = %q", got.Name, tt.def.Name)
			}
			if got.Description != tt.def.Description {
				t.Errorf("description: got = %q, wanted = %q", got.Description, tt.def.Description)
			}

			schema, ok := got.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("input schema type: got = %T, wanted = map[string]any", got.InputSchema)
			}
			if schema["type"] != "object" {
				t.Errorf("schema type: got = %v, wanted = %q", schema["type"], "object")
			}

			props, _ := schema["properties"].(map[string]any)
			if got, want := len(props), len(tt.def.Parameters); got != want {
				t.Errorf("properties count: got = %d, wanted = %d", got, want)
			}

			for _, p := range tt.def.Parameters {
				prop, ok := props[p.Name].(map[string]any)
				if !ok {
					t.Errorf("missing property %q", p.Name)
					continue
				}
				if prop["type"] != p.Type {
					t.Errorf("property %q type: got = %v, wanted = %q", p.Name, prop["type"], p.Type)
				}
			}
		})
	}
}

func TestToCallToolResult(t *testing.T) {
	tests := []struct {
		name    string
		resp    map[string]any
		isError bool
	}{{
		name:    "nil response",
		resp:    nil,
		isError: false,
	}, {
		name:    "success response",
		resp:    map[string]any{"data": "hello"},
		isError: false,
	}, {
		name:    "error response",
		resp:    map[string]any{"error": "something went wrong"},
		isError: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toCallToolResult(tt.resp)
			if result.IsError != tt.isError {
				t.Errorf("IsError: got = %v, wanted = %v", result.IsError, tt.isError)
			}
			if len(result.Content) != 1 {
				t.Fatalf("content length: got = %d, wanted = 1", len(result.Content))
			}
		})
	}
}

func TestMcpHandler(t *testing.T) {
	sentinel := fmt.Sprintf("test-echo-%d", rand.Int64())

	tool := toolcall.Tool[any]{
		Def: toolcall.Definition{
			Name:        "echo",
			Description: "Echo a message",
			Parameters: []toolcall.Parameter{{
				Name:        "message",
				Type:        "string",
				Description: "Message to echo",
				Required:    true,
			}},
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[any], _ *any) map[string]any {
			msg, errResp := toolcall.Param[string](call, trace, "message")
			if errResp != nil {
				return errResp
			}
			return map[string]any{"echo": msg}
		},
	}

	args, err := json.Marshal(map[string]any{"message": sentinel})
	if err != nil {
		t.Fatal(err)
	}

	handler := mcpHandler(tool)
	result, err := handler(t.Context(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "echo",
			Arguments: args,
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected error result")
	}

	// Verify the response contains our sentinel value.
	data, err := json.Marshal(result.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	text, ok := content["text"].(string)
	if !ok {
		t.Fatalf("content text type: got = %T, wanted = string", content["text"])
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to unmarshal response text: %v", err)
	}
	if resp["echo"] != sentinel {
		t.Errorf("echo: got = %v, wanted = %v", resp["echo"], sentinel)
	}
}

func TestMcpHandlerMissingParam(t *testing.T) {
	tool := toolcall.Tool[any]{
		Def: toolcall.Definition{
			Name:        "echo",
			Description: "Echo a message",
			Parameters: []toolcall.Parameter{{
				Name:        "message",
				Type:        "string",
				Description: "Message to echo",
				Required:    true,
			}},
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[any], _ *any) map[string]any {
			msg, errResp := toolcall.Param[string](call, trace, "message")
			if errResp != nil {
				return errResp
			}
			return map[string]any{"echo": msg}
		},
	}

	// Call with empty arguments — should produce an error result.
	args, err := json.Marshal(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}

	handler := mcpHandler(tool)
	result, err := handler(t.Context(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "echo",
			Arguments: args,
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("result.IsError for missing parameter: got = false, want = true")
	}
}

// TestMcpHandlerCompletesTrace checks the per-call trace is recorded — so
// tool calls served over MCP reach the tracer instead of leaking open spans —
// and that the recorded trace carries the call's real outcome: failed tool
// responses and response serialization failures record a non-nil error.
func TestMcpHandlerCompletesTrace(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		resp    map[string]any
		wantErr bool
	}{{
		name: "success",
		resp: map[string]any{"pong": true},
	}, {
		name:    "tool error response",
		resp:    map[string]any{"error": "boom"},
		wantErr: true,
	}, {
		name:    "unserializable response",
		resp:    map[string]any{"bad": make(chan int)},
		wantErr: true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var recorded atomic.Int32
			ctx := agenttrace.WithTracer(t.Context(), agenttrace.ByCode[any](func(tr *agenttrace.Trace[any]) {
				recorded.Add(1)
				if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "ping" {
					t.Errorf("trace tool calls: %+v", tr.ToolCalls)
				}
				if gotErr := tr.Error != nil; gotErr != tt.wantErr {
					t.Errorf("trace error: got = %v, want error = %v", tr.Error, tt.wantErr)
				}
			}))
			handler := mcpHandler(toolcall.Tool[any]{
				Def: toolcall.Definition{Name: "ping"},
				Handler: func(_ context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[any], _ *any) map[string]any {
					tc := trace.StartToolCall(call.ID, call.Name, call.Args)
					tc.Complete(tt.resp, nil)
					return tt.resp
				},
			})
			result, err := handler(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "ping"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != tt.wantErr {
				t.Errorf("result.IsError: got = %v, want = %v", result.IsError, tt.wantErr)
			}
			if got := recorded.Load(); got != 1 {
				t.Errorf("traces recorded: got = %d, want = 1", got)
			}
		})
	}
}

func TestFromTools(t *testing.T) {
	tools := map[string]toolcall.Tool[any]{
		"tool_a": {
			Def: toolcall.Definition{
				Name:        "tool_a",
				Description: "First tool",
			},
			Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[any], _ *any) map[string]any {
				return map[string]any{"ok": true}
			},
		},
		"tool_b": {
			Def: toolcall.Definition{
				Name:        "tool_b",
				Description: "Second tool",
			},
			Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[any], _ *any) map[string]any {
				return map[string]any{"ok": true}
			},
		},
	}

	server := FromTools("test", tools, WithVersion("1.0.0"), WithInstructions("test instructions"))
	if server == nil {
		t.Fatal("FromTools returned nil")
	}
}

// TestMCPToolAnnotations_NilNotEmitted verifies that when a Definition carries
// nil Destructive/OpenWorld (unset, MCP default true), mcpTool does not emit
// an explicit &false pointer — which would override the default for downstream
// clients.
func TestMCPToolAnnotations_NilNotEmitted(t *testing.T) {
	def := toolcall.Definition{
		Name:        "test",
		Description: "test",
		Annotations: &toolcall.ToolAnnotations{
			ReadOnly:    true,
			Idempotent:  true,
			Destructive: nil, // unset — MCP default is true
			OpenWorld:   nil, // unset — MCP default is true
		},
	}
	tool := mcpTool(def)
	if tool.Annotations == nil {
		t.Fatal("Annotations: got = nil")
	}
	if tool.Annotations.DestructiveHint != nil {
		t.Errorf("DestructiveHint: got = %v, wanted = nil (preserve default-true)", tool.Annotations.DestructiveHint)
	}
	if tool.Annotations.OpenWorldHint != nil {
		t.Errorf("OpenWorldHint: got = %v, wanted = nil (preserve default-true)", tool.Annotations.OpenWorldHint)
	}
	if !tool.Annotations.ReadOnlyHint {
		t.Error("ReadOnlyHint: got = false, wanted = true")
	}
	if !tool.Annotations.IdempotentHint {
		t.Error("IdempotentHint: got = false, wanted = true")
	}
}

// staticProvider serves a fixed tool map.
type staticProvider struct {
	tools map[string]toolcall.Tool[any]
}

func (p staticProvider) Tools(_ context.Context, _ struct{}) (map[string]toolcall.Tool[any], error) {
	return p.tools, nil
}

func TestNewServerServesCalls(t *testing.T) {
	provider := staticProvider{tools: map[string]toolcall.Tool[any]{
		"echo": {
			Def: toolcall.Definition{
				Name:        "echo",
				Description: "Echo a word",
				Parameters:  []toolcall.Parameter{{Name: "word", Type: "string", Required: true}},
			},
			Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[any], _ *any) map[string]any {
				return map[string]any{"echo": call.Args["word"]}
			},
		},
	}}
	server, err := NewServer(t.Context(), "test", provider, struct{}{}, WithVersion("1.2.3"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	if got := cs.InitializeResult().ServerInfo.Version; got != "1.2.3" {
		t.Errorf("server version: got = %q, want = %q", got, "1.2.3")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"word": "hi"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool: tool error %v", res.Content)
	}
	if got, want := res.Content[0].(*mcp.TextContent).Text, `{"echo":"hi"}`; got != want {
		t.Errorf("result: got = %s, want = %s", got, want)
	}
}

type failingProvider struct{}

func (failingProvider) Tools(context.Context, struct{}) (map[string]toolcall.Tool[any], error) {
	return nil, fmt.Errorf("no tools today")
}

func TestNewServerListError(t *testing.T) {
	if _, err := NewServer(t.Context(), "test", failingProvider{}, struct{}{}); err == nil {
		t.Error("NewServer: got = nil error, want the provider's")
	}
}
