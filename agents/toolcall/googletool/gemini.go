/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"context"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/agenttrace"
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
	var zero T
	value, exists := call.Args[name]
	if !exists {
		return zero, &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: map[string]any{
				"error": fmt.Sprintf("%s parameter is required", name),
			},
		}
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	switch any(zero).(type) {
	case int:
		if floatVal, ok := value.(float64); ok {
			return any(int(floatVal)).(T), nil
		}
	case int32:
		if floatVal, ok := value.(float64); ok {
			return any(int32(floatVal)).(T), nil
		}
	case int64:
		if floatVal, ok := value.(float64); ok {
			return any(int64(floatVal)).(T), nil
		}
	}

	return zero, &genai.FunctionResponse{
		ID:   call.ID,
		Name: call.Name,
		Response: map[string]any{
			"error": fmt.Sprintf("%s parameter must be of type %T, got %T", name, zero, value),
		},
	}
}

// OptionalParam extracts an optional parameter from a Gemini function call.
// Returns the default value if the parameter doesn't exist, or a FunctionResponse error if type conversion fails.
func OptionalParam[T any](call *genai.FunctionCall, name string, defaultValue T) (T, *genai.FunctionResponse) {
	value, exists := call.Args[name]
	if !exists {
		return defaultValue, nil
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	var zero T
	switch any(zero).(type) {
	case int:
		if floatVal, ok := value.(float64); ok {
			return any(int(floatVal)).(T), nil
		}
	case int32:
		if floatVal, ok := value.(float64); ok {
			return any(int32(floatVal)).(T), nil
		}
	case int64:
		if floatVal, ok := value.(float64); ok {
			return any(int64(floatVal)).(T), nil
		}
	}

	return zero, &genai.FunctionResponse{
		ID:   call.ID,
		Name: call.Name,
		Response: map[string]any{
			"error": fmt.Sprintf("%s parameter must be of type %T, got %T", name, zero, value),
		},
	}
}

// Error creates a FunctionResponse with an error message
func Error(call *genai.FunctionCall, format string, args ...any) *genai.FunctionResponse {
	return &genai.FunctionResponse{
		ID:   call.ID,
		Name: call.Name,
		Response: map[string]any{
			"error": fmt.Sprintf(format, args...),
		},
	}
}

// ErrorWithContext creates a FunctionResponse with an error and additional context
func ErrorWithContext(call *genai.FunctionCall, err error, context map[string]any) *genai.FunctionResponse {
	response := map[string]any{
		"error": err.Error(),
	}
	// Add context fields
	maps.Copy(response, context)
	return &genai.FunctionResponse{
		ID:       call.ID,
		Name:     call.Name,
		Response: response,
	}
}
