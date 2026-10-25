/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Option configures the server.
type Option func(*config)

type config struct {
	version      string
	instructions string
}

// WithVersion sets the server version reported in MCP initialization.
func WithVersion(version string) Option {
	return func(c *config) { c.version = version }
}

// WithInstructions sets the server instructions in MCP initialization,
// which tell an LLM client what the server is for.
func WithInstructions(instructions string) Option {
	return func(c *config) { c.instructions = instructions }
}

// NewServer lists provider's tools with cb and serves them.
func NewServer[CB any](ctx context.Context, name string, provider toolcall.ToolProvider[any, CB], cb CB, opts ...Option) (*mcp.Server, error) {
	tools, err := provider.Tools(ctx, cb)
	if err != nil {
		return nil, fmt.Errorf("listing tools: %w", err)
	}
	return FromTools(name, tools, opts...), nil
}

// FromTools serves tools.
func FromTools(name string, tools map[string]toolcall.Tool[any], opts ...Option) *mcp.Server {
	cfg := config{version: "0.0.1"}
	for _, opt := range opts {
		opt(&cfg)
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Version: cfg.version,
	}, &mcp.ServerOptions{
		Instructions: cfg.instructions,
	})
	for _, tool := range tools {
		server.AddTool(mcpTool(tool.Def), mcpHandler(tool))
	}
	return server
}

// mcpTool converts a toolcall.Definition to an mcp.Tool.
func mcpTool(def toolcall.Definition) *mcp.Tool {
	props := make(map[string]any, len(def.Parameters))
	var required []string

	for _, p := range def.Parameters {
		props[p.Name] = toolcall.ParameterToMap(p)
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	// Only include "required" when non-empty; a null value is invalid JSON Schema
	// and causes some MCP clients (e.g. Claude Code) to reject the tool list.
	if len(required) > 0 {
		schema["required"] = required
	}
	// Preserve the top-level description (proto message description) if present.
	if def.InputSchemaDescription != "" {
		schema["description"] = def.InputSchemaDescription
	}
	// Preserve "$defs" so that "$ref" references within parameters remain valid.
	if len(def.InputSchemaDefs) > 0 {
		defs := make(map[string]any, len(def.InputSchemaDefs))
		for name, s := range def.InputSchemaDefs {
			defs[name] = toolcall.SchemaToMap(s)
		}
		schema["$defs"] = defs
	}
	// Preserve any other top-level keywords (e.g. "$schema" meta-schema URL).
	maps.Copy(schema, def.InputSchemaExtensions)

	t := &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: schema,
	}
	if def.OutputSchema != nil {
		t.OutputSchema = toolcall.SchemaToMap(def.OutputSchema)
	}
	if a := def.Annotations; a != nil {
		t.Annotations = &mcp.ToolAnnotations{
			ReadOnlyHint:    a.ReadOnly,
			DestructiveHint: a.Destructive,
			IdempotentHint:  a.Idempotent,
			OpenWorldHint:   a.OpenWorld,
		}
	}
	return t
}

// mcpHandler creates an mcp.ToolHandler from a unified tool.
func mcpHandler(tool toolcall.Tool[any]) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if req.Params.Arguments != nil {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult("failed to parse tool arguments: %v", err), nil
			}
		}
		if args == nil {
			args = make(map[string]any)
		}

		trace, done := agenttrace.StartTrace[any](ctx, "mcp-tool-call")
		var result any
		resp := tool.Handler(ctx, toolcall.ToolCall{
			// TODO: go-sdk does not surface the JSON-RPC request id to tool
			// handlers, so the tool name stands in for the call id. Calls
			// to one tool within a trace are therefore indistinguishable by
			// ID; each MCP call gets its own trace, which bounds the damage.
			ID:   req.Params.Name,
			Name: req.Params.Name,
			Args: args,
		}, trace, &result)

		// Complete the trace with the call's real outcome so failure
		// monitoring on root traces sees tool errors and response
		// serialization failures, not a blanket success.
		callResult := toCallToolResult(resp)
		traceErr := callResult.GetError()
		if callResult.IsError && traceErr == nil {
			traceErr = fmt.Errorf("tool %q failed: %v", req.Params.Name, resp["error"])
		}
		done(result, traceErr)

		return callResult, nil
	}
}

// toCallToolResult converts a tool handler's map[string]any response to an
// mcp.CallToolResult.
func toCallToolResult(resp map[string]any) *mcp.CallToolResult {
	if resp == nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "{}"}},
		}
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return errorResult("failed to marshal tool response: %v", err)
	}

	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}
	if _, hasError := resp["error"]; hasError {
		result.IsError = true
	}
	return result
}

func errorResult(format string, args ...any) *mcp.CallToolResult {
	result := &mcp.CallToolResult{}
	result.SetError(fmt.Errorf(format, args...))
	return result
}
