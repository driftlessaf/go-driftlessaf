/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"errors"
	"net/http"

	"github.com/openai/openai-go"
)

// isRetryableOpenAIError checks if an error is a retryable OpenAI API error.
// Returns true for rate limit, overloaded, and transient server errors.
func isRetryableOpenAIError(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}

	return false
}
