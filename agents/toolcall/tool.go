/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// ToolCall is a provider-independent representation of a tool call.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// Definition describes a tool's schema (name, description, parameters).
type Definition struct {
	Name        string
	Description string
	Parameters  []Parameter
}

// Parameter describes a single tool parameter.
type Parameter struct {
	Name        string
	Type        string // "string", "integer", "boolean", "number"
	Description string
	Required    bool
}

// Tool defines a tool once with a single handler that works with both providers.
type Tool[Resp any] struct {
	Def     Definition
	Handler func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], result *Resp) map[string]any
}

// ClaudeMetadata converts the unified tool to Claude-specific metadata.
func (t Tool[Resp]) ClaudeMetadata() claudetool.Metadata[Resp] {
	return claudetool.Metadata[Resp]{
		Definition: t.claudeToolParam(),
		Handler:    t.claudeHandler(),
	}
}

// GoogleMetadata converts the unified tool to Google-specific metadata.
func (t Tool[Resp]) GoogleMetadata() googletool.Metadata[Resp] {
	return googletool.Metadata[Resp]{
		Definition: t.googleToolParam(),
		Handler:    t.googleHandler(),
	}
}

func (t Tool[Resp]) claudeToolParam() anthropic.ToolParam {
	props := make(map[string]any, len(t.Def.Parameters))
	var required []string
	for _, p := range t.Def.Parameters {
		props[p.Name] = map[string]any{
			"type":        p.Type,
			"description": p.Description,
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return anthropic.ToolParam{
		Name:        t.Def.Name,
		Description: anthropic.String(t.Def.Description),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type:       "object",
			Properties: props,
			Required:   required,
		},
	}
}

func (t Tool[Resp]) googleToolParam() *genai.FunctionDeclaration {
	props := make(map[string]*genai.Schema, len(t.Def.Parameters))
	var required []string
	for _, p := range t.Def.Parameters {
		props[p.Name] = &genai.Schema{
			Type:        genai.Type(p.Type),
			Description: p.Description,
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return &genai.FunctionDeclaration{
		Name:        t.Def.Name,
		Description: t.Def.Description,
		Parameters: &genai.Schema{
			Type:       "object",
			Properties: props,
			Required:   required,
		},
	}
}

func (t Tool[Resp]) claudeHandler() func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[Resp], *Resp) map[string]any {
	return func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Resp], result *Resp) map[string]any {
		var args map[string]any
		if err := json.Unmarshal(toolUse.Input, &args); err != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("failed to parse params"))
			return params.Error("Failed to parse tool input: %v", err)
		}
		call := ToolCall{
			ID:   toolUse.ID,
			Name: toolUse.Name,
			Args: args,
		}
		return t.Handler(ctx, call, trace, result)
	}
}

func (t Tool[Resp]) googleHandler() func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], result *Resp) *genai.FunctionResponse {
		tc := ToolCall{
			ID:   call.ID,
			Name: call.Name,
			Args: call.Args,
		}
		resp := t.Handler(ctx, tc, trace, result)
		if resp == nil {
			return &genai.FunctionResponse{
				ID:       call.ID,
				Name:     call.Name,
				Response: map[string]any{},
			}
		}
		return &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: resp,
		}
	}
}

// Param extracts a required parameter from the tool call args.
// On error, records a bad tool call on the trace and returns an error response.
func Param[T any](call ToolCall, trace interface {
	BadToolCall(string, string, map[string]any, error)
}, name string) (T, map[string]any) {
	v, err := params.Extract[T](call.Args, name)
	if err != nil {
		trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("missing %s parameter", name))
		return v, params.Error("%s", err)
	}
	return v, nil
}

// OptionalParam extracts an optional parameter from the tool call args.
func OptionalParam[T any](call ToolCall, name string, defaultValue T) (T, map[string]any) {
	v, err := params.ExtractOptional[T](call.Args, name, defaultValue)
	if err != nil {
		return v, params.Error("%s", err)
	}
	return v, nil
}
