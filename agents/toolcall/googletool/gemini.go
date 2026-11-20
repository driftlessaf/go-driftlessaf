/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"context"
	"slices"

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

	// LogPolicy is copied from toolcall.Definition by FromTool. A caller
	// assembling Metadata by hand must copy it too, or arguments are logged.
	LogPolicy *toolcall.ArgLogPolicy

	// AfterSiblings is copied from toolcall.Definition by FromTool: the
	// executor runs the tool's calls after the turn's other calls, before
	// its submit.
	AfterSiblings bool

	// InjectedReasoning reports that Definition carries the reasoning
	// parameter FromTool injects, which WithoutReasoning removes.
	InjectedReasoning bool
}

// WithoutReasoning returns m with the injected reasoning parameter removed
// from its definition. Metadata without one is returned unchanged, so a
// hand-built tool's own reasoning parameter is never dropped. The definition
// is copied, never modified in place, and the handler is unchanged: tool
// handlers do not read the injected parameter.
func (m Metadata[Response]) WithoutReasoning() Metadata[Response] {
	if !m.InjectedReasoning || m.Definition == nil || m.Definition.Parameters == nil {
		return m
	}
	schema := *m.Definition.Parameters
	schema.Properties = make(map[string]*genai.Schema, len(m.Definition.Parameters.Properties))
	for k, v := range m.Definition.Parameters.Properties {
		if k != "reasoning" {
			schema.Properties[k] = v
		}
	}
	notReasoning := func(s []string) []string {
		return slices.DeleteFunc(slices.Clone(s), func(r string) bool { return r == "reasoning" })
	}
	schema.Required = notReasoning(schema.Required)
	schema.PropertyOrdering = notReasoning(schema.PropertyOrdering)
	def := *m.Definition
	def.Parameters = &schema
	m.Definition = &def
	m.InjectedReasoning = false
	return m
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

	// NoReasoning is the same submit tool without its reasoning parameter,
	// for models whose provider facts set OmitToolReasoning. Nil when the
	// tool has no reasoning parameter or has no such variant; the executor
	// then sends Definition as it is.
	NoReasoning *SubmitMetadata[Response]
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
		Definition:        toolParam(t.Def),
		Handler:           handler(t),
		LogPolicy:         t.Def.LogPolicy,
		AfterSiblings:     t.Def.AfterSiblings,
		InjectedReasoning: true,
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
