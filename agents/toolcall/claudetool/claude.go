/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"context"
	"encoding/json"
	"errors"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/anthropics/anthropic-sdk-go"
)

// Metadata describes a tool available to the Claude agent.
type Metadata[Response any] struct {
	// Definition is the tool definition for Claude.
	Definition anthropic.ToolParam

	// Handler processes the tool call.
	// If the handler sets *result to a non-zero value, the executor will immediately exit with that response.
	Handler func(
		ctx context.Context,
		toolUse anthropic.ToolUseBlock,
		trace *agenttrace.Trace[Response],
		result *Response,
	) map[string]any
}

// Error creates an error response map for Claude tool calls
func Error(format string, args ...any) map[string]any {
	return params.Error(format, args...)
}

// ErrorWithContext creates an error response with additional context
func ErrorWithContext(err error, context map[string]any) map[string]any {
	return params.ErrorWithContext(err, context)
}

// FromTool converts a unified tool to Claude-specific metadata.
func FromTool[Resp any](t toolcall.Tool[Resp]) Metadata[Resp] {
	return Metadata[Resp]{
		Definition: toolParam(t.Def),
		Handler:    handler(t),
	}
}

// Map converts a unified tool map to Claude-specific metadata.
func Map[Resp any](tools map[string]toolcall.Tool[Resp]) map[string]Metadata[Resp] {
	m := make(map[string]Metadata[Resp], len(tools))
	for name, t := range tools {
		m[name] = FromTool(t)
	}
	return m
}

func toolParam(def toolcall.Definition) anthropic.ToolParam {
	props := make(map[string]any, len(def.Parameters))
	var required []string
	for _, p := range def.Parameters {
		props[p.Name] = map[string]any{
			"type":        p.Type,
			"description": p.Description,
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return anthropic.ToolParam{
		Name:        def.Name,
		Description: anthropic.String(def.Description),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type:       "object",
			Properties: props,
			Required:   required,
		},
	}
}

func handler[Resp any](t toolcall.Tool[Resp]) func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], result *Resp) map[string]any {
		var args map[string]any
		if err := json.Unmarshal(toolUse.Input, &args); err != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return params.Error("Failed to parse tool input: %v", err)
		}
		call := toolcall.ToolCall{
			ID:   toolUse.ID,
			Name: toolUse.Name,
			Args: args,
		}
		return t.Handler(ctx, call, trace, result)
	}
}
