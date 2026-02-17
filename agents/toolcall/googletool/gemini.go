/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"context"

	"chainguard.dev/driftlessaf/agents/agenttrace"
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
