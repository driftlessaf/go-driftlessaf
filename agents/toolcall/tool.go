/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
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

// Tool defines a tool once with a single handler that works with any provider.
type Tool[Resp any] struct {
	Def     Definition
	Handler func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], result *Resp) map[string]any
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
