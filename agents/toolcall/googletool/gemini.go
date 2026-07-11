/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"context"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"google.golang.org/genai"
)

// Metadata describes a tool available to the Google AI agent.
type Metadata[Response any] struct {
	// Definition is the Google AI tool definition.
	Definition *genai.FunctionDeclaration

	// Handler is the function that processes tool calls.
	// It receives the context, tool call, trace, and a result pointer.
	// If the handler sets *result to a non-zero value, the executor will immediately exit with that response.
	Handler func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response], result *Response) *genai.FunctionResponse
}

// SubmitMetadata describes the terminal submit tool for the Google executor.
// It is registered via the executor's WithSubmitResultProvider option rather
// than the regular tool map because the submit tool is special: its accepted
// outcome — once the executor's result validators pass — becomes the run's
// final result and ends the agent loop.
type SubmitMetadata[Response any] struct {
	// Definition is the Google AI tool definition.
	Definition *genai.FunctionDeclaration

	// Handler parses a submit tool call into a SubmitOutcome. It performs no
	// side effects on the run: committing the response is the executor's
	// decision. The handler records the trace tool call for parameter and
	// parse failures (Accepted=false); the executor records accepted calls so
	// their completion reflects the validation verdict.
	Handler func(
		ctx context.Context,
		call *genai.FunctionCall,
		trace *agenttrace.Trace[Response],
	) toolcall.SubmitOutcome[Response]
}

// Param extracts a parameter from a Gemini function call with type safety.
// Returns the extracted value or a FunctionResponse error that can be sent back to the model.
func Param[T any](call *genai.FunctionCall, name string) (T, *genai.FunctionResponse) {
	v, err := params.Extract[T](call.Args, name)
	if err != nil {
		return v, &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: params.Error("%s", err),
		}
	}
	return v, nil
}

// OptionalParam extracts an optional parameter from a Gemini function call.
// Returns the default value if the parameter doesn't exist, or a FunctionResponse error if type conversion fails.
func OptionalParam[T any](call *genai.FunctionCall, name string, defaultValue T) (T, *genai.FunctionResponse) {
	v, err := params.ExtractOptional[T](call.Args, name, defaultValue)
	if err != nil {
		return v, &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: params.Error("%s", err),
		}
	}
	return v, nil
}

// Error creates a FunctionResponse with an error message
func Error(call *genai.FunctionCall, format string, args ...any) *genai.FunctionResponse {
	return &genai.FunctionResponse{
		ID:       call.ID,
		Name:     call.Name,
		Response: params.Error(format, args...),
	}
}

// ErrorWithContext creates a FunctionResponse with an error and additional context
func ErrorWithContext(call *genai.FunctionCall, err error, context map[string]any) *genai.FunctionResponse {
	return &genai.FunctionResponse{
		ID:       call.ID,
		Name:     call.Name,
		Response: params.ErrorWithContext(err, context),
	}
}

// FromTool converts a unified tool to Google-specific metadata.
func FromTool[Resp any](t toolcall.Tool[Resp]) Metadata[Resp] {
	return Metadata[Resp]{
		Definition: toolParam(t.Def),
		Handler:    handler(t),
	}
}

// Map converts a unified tool map to Google-specific metadata.
func Map[Resp any](tools map[string]toolcall.Tool[Resp]) map[string]Metadata[Resp] {
	m := make(map[string]Metadata[Resp], len(tools))
	for name, t := range tools {
		m[name] = FromTool(t)
	}
	return m
}

func toolParam(def toolcall.Definition) *genai.FunctionDeclaration {
	props := make(map[string]*genai.Schema, len(def.Parameters)+1)
	required := []string{"reasoning"}

	// Auto-inject reasoning as the first parameter.
	props["reasoning"] = &genai.Schema{
		Type:        "string",
		Description: "Explain why you are making this tool call and what you hope to accomplish.",
	}

	for _, p := range def.Parameters {
		props[p.Name] = paramToGenAI(p)
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return &genai.FunctionDeclaration{
		Name:        def.Name,
		Description: def.Description,
		Parameters: &genai.Schema{
			Type:       "object",
			Properties: props,
			Required:   required,
		},
	}
}

// paramToGenAI converts a Parameter to a *genai.Schema. Genai supports a
// subset of JSON Schema, so fields without a genai equivalent are silently
// dropped (e.g. OneOf, AllOf, pattern, numeric constraints).
func paramToGenAI(p toolcall.Parameter) *genai.Schema {
	s := &genai.Schema{
		Type:        genai.Type(p.Type),
		Description: p.Description,
	}
	if p.Items != nil {
		s.Items = schemaToGenAI(p.Items)
	}
	if len(p.Properties) > 0 {
		s.Properties = make(map[string]*genai.Schema, len(p.Properties))
		for name, prop := range p.Properties {
			s.Properties[name] = schemaToGenAI(prop)
		}
		s.Required = p.PropertyRequired
	}
	if len(p.Enum) > 0 {
		s.Enum = make([]string, 0, len(p.Enum))
		for _, v := range p.Enum {
			if str, ok := v.(string); ok {
				s.Enum = append(s.Enum, str)
			}
		}
	}
	return s
}

// schemaToGenAI recursively converts a *toolcall.Schema to *genai.Schema.
func schemaToGenAI(ts *toolcall.Schema) *genai.Schema {
	if ts == nil || ts.False {
		return nil
	}
	s := &genai.Schema{
		Type:        genai.Type(ts.Type),
		Description: ts.Description,
	}
	if ts.Items != nil {
		s.Items = schemaToGenAI(ts.Items)
	}
	if len(ts.Properties) > 0 {
		s.Properties = make(map[string]*genai.Schema, len(ts.Properties))
		for name, prop := range ts.Properties {
			s.Properties[name] = schemaToGenAI(prop)
		}
		s.Required = ts.Required
	}
	if len(ts.Enum) > 0 {
		s.Enum = make([]string, 0, len(ts.Enum))
		for _, v := range ts.Enum {
			if str, ok := v.(string); ok {
				s.Enum = append(s.Enum, str)
			}
		}
	}
	return s
}

func handler[Resp any](t toolcall.Tool[Resp]) func(context.Context, *genai.FunctionCall, *agenttrace.Trace[Resp], *Resp) *genai.FunctionResponse {
	return func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Resp], result *Resp) *genai.FunctionResponse {
		resp := t.Handler(ctx, toolcall.ToolCall{
			ID:   call.ID,
			Name: call.Name,
			Args: call.Args,
		}, trace, result)
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
