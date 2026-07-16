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

// SubmitMetadata describes the terminal submit tool for the Claude executor.
// It is registered via the executor's WithSubmitResultProvider option rather
// than the regular tool map because the submit tool is special: its accepted
// outcome — once the executor's result validators pass — becomes the run's
// final result and ends the agent loop.
type SubmitMetadata[Response any] struct {
	// Definition is the tool definition for Claude.
	Definition anthropic.ToolParam

	// Handler parses a submit tool call into a SubmitOutcome. It performs no
	// side effects on the run: committing the response is the executor's
	// decision. The handler records the trace tool call for parameter and
	// parse failures (Accepted=false); the executor records accepted calls so
	// their completion reflects the validation verdict.
	Handler func(
		ctx context.Context,
		toolUse anthropic.ToolUseBlock,
		trace *agenttrace.Trace[Response],
	) toolcall.SubmitOutcome[Response]
}

// Error creates an error response map for Claude tool calls
func Error(format string, args ...any) map[string]any {
	return params.Error(format, args...)
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
	props := make(map[string]any, len(def.Parameters)+1)
	required := []string{"reasoning"}

	// Auto-inject reasoning as the first parameter.
	props["reasoning"] = map[string]any{
		"type":        "string",
		"description": "Explain why you are making this tool call and what you hope to accomplish.",
	}

	for _, p := range def.Parameters {
		props[p.Name] = toolcall.ParameterToMap(p)
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
		return t.Handler(ctx, toolcall.ToolCall{
			ID:   toolUse.ID,
			Name: toolUse.Name,
			Args: args,
		}, trace, result)
	}
}
