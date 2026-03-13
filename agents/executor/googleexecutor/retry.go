/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
)

// isRetryableVertexError checks if an error is a retryable Vertex AI error.
// Returns true for rate limit, quota exhaustion, and transient server errors.
func isRetryableVertexError(err error) bool {
	if err == nil {
		return false
	}

	// Check for googleapi.Error with HTTP status codes
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusTooManyRequests, // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout,      // 504
			499:                            // Client Closed Request / Cancelled (Vertex AI DSQ)
			return true
		}
	}

	// Fall back to string matching for errors not wrapped as googleapi.Error
	// (e.g. gRPC errors that surface as plain strings)
	errStr := err.Error()
	return strings.Contains(errStr, "Resource exhausted") ||
		strings.Contains(errStr, "ResourceExhausted") ||
		strings.Contains(errStr, "RESOURCE_EXHAUSTED") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "Overloaded") ||
		strings.Contains(errStr, "quota exceeded") ||
		strings.Contains(errStr, "Internal error") ||
		strings.Contains(errStr, "server error") ||
		strings.Contains(errStr, "CANCELLED")
}
