/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"context"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/agenttrace"
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
	return map[string]any{
		"error": fmt.Sprintf(format, args...),
	}
}

// ErrorWithContext creates an error response with additional context
func ErrorWithContext(err error, context map[string]any) map[string]any {
	response := map[string]any{
		"error": err.Error(),
	}
	// Add context fields
	maps.Copy(response, context)
	return response
}
