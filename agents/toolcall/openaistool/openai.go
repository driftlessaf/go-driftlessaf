/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaistool

import (
	"context"
	"encoding/json"
	"errors"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// Metadata describes a tool available to the OpenAI agent.
type Metadata[Response any] struct {
	// Definition is the tool definition for OpenAI.
	Definition openai.ChatCompletionToolParam

	// Handler processes the tool call.
	// If the handler sets *result to a non-zero value, the executor will immediately exit with that response.
	Handler func(
		ctx context.Context,
		toolCall openai.ChatCompletionMessageToolCall,
		trace *agenttrace.Trace[Response],
		result *Response,
	) map[string]any
}

// Error creates an error response map for OpenAI tool calls.
func Error(format string, args ...any) map[string]any {
	return params.Error(format, args...)
}

// ErrorWithContext creates an error response with additional context.
func ErrorWithContext(err error, context map[string]any) map[string]any {
	return params.ErrorWithContext(err, context)
}

// FromTool converts a unified tool to OpenAI-specific metadata.
func FromTool[Resp any](t toolcall.Tool[Resp]) Metadata[Resp] {
	return Metadata[Resp]{
		Definition: toolParam(t.Def),
		Handler:    handler(t),
	}
}

// Map converts a unified tool map to OpenAI-specific metadata.
func Map[Resp any](tools map[string]toolcall.Tool[Resp]) map[string]Metadata[Resp] {
	m := make(map[string]Metadata[Resp], len(tools))
	for name, t := range tools {
		m[name] = FromTool(t)
	}
	return m
}

func toolParam(def toolcall.Definition) openai.ChatCompletionToolParam {
	props := make(map[string]any, len(def.Parameters)+1)
	required := []string{"reasoning"}

	// Auto-inject reasoning as the first parameter.
	props["reasoning"] = map[string]any{
		"type":        "string",
		"description": "Explain why you are making this tool call and what you hope to accomplish.",
	}

	for _, p := range def.Parameters {
		props[p.Name] = map[string]any{
			"type":        p.Type,
			"description": p.Description,
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}

	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        def.Name,
			Description: param.NewOpt(def.Description),
			Parameters: shared.FunctionParameters{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

func handler[Resp any](t toolcall.Tool[Resp]) func(context.Context, openai.ChatCompletionMessageToolCall, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, tc openai.ChatCompletionMessageToolCall, trace *agenttrace.Trace[Resp], result *Resp) map[string]any {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, map[string]any{"arguments": tc.Function.Arguments}, errors.New("failed to parse params"))
			return params.Error("Failed to parse tool arguments: %v", err)
		}
		return t.Handler(ctx, toolcall.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: args,
		}, trace, result)
	}
}
