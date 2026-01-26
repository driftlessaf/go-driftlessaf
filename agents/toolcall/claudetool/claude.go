/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"fmt"
	"maps"
)

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
