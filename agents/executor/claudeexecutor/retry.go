/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
)

// isRetryableClaudeError checks if an error is a retryable Claude API error.
// Returns true for rate limit, overloaded, and transient server errors.
func isRetryableClaudeError(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 429, 503, 504, 529:
			return true
		}
	}
	return false
}
