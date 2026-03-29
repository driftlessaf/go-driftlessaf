/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// isRetryableClaudeError checks if an error is a retryable Claude API error.
// Returns true for rate limit, overloaded, and transient server errors.
//
// This handles two distinct error shapes from the Anthropic SDK:
//  1. Structured *anthropic.Error with StatusCode (non-streaming API errors)
//  2. Plain fmt.Errorf from SSE streaming errors that embed the raw JSON
//     (e.g. "received error while streaming: {"type":"error","error":{"type":"overloaded_error",...}}")
func isRetryableClaudeError(err error) bool {
	if err == nil {
		return false
	}

	// Check structured API errors (non-streaming path)
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
			return true
		default:
			return false
		}
	}

	// Check SSE streaming errors which are plain fmt.Errorf with raw JSON.
	// The SDK's ssestream package emits: "received error while streaming: <json>"
	// where the JSON contains the error type string. The retryable types per
	// https://docs.anthropic.com/en/api/errors are:
	//   - 429: "rate_limit_error"
	//   - 500: "api_error"
	//   - 529: "overloaded_error"
	errStr := err.Error()
	return strings.Contains(errStr, "overloaded_error") ||
		strings.Contains(errStr, "rate_limit_error") ||
		strings.Contains(errStr, "api_error")
}
