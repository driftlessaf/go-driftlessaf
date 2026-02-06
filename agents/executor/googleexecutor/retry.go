/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"strings"
)

// isRetryableVertexError checks if an error is a retryable Vertex AI error.
// Returns true for rate limit, quota exhaustion, and transient server errors.
func isRetryableVertexError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "Resource exhausted") ||
		strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "RESOURCE_EXHAUSTED") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "Overloaded") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "quota exceeded") ||
		strings.Contains(errStr, "Internal error") ||
		strings.Contains(errStr, "server error")
}
