/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"
)

func TestIsRetryableVertexError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		// googleapi.Error with HTTP status codes
		{name: "429 googleapi", err: &googleapi.Error{Code: http.StatusTooManyRequests, Message: "rate limited"}, want: true},
		{name: "499 googleapi", err: &googleapi.Error{Code: 499, Message: "The operation was cancelled."}, want: true},
		{name: "500 googleapi", err: &googleapi.Error{Code: http.StatusInternalServerError, Message: "internal"}, want: true},
		{name: "502 googleapi", err: &googleapi.Error{Code: http.StatusBadGateway, Message: "bad gateway"}, want: true},
		{name: "503 googleapi", err: &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "unavailable"}, want: true},
		{name: "504 googleapi", err: &googleapi.Error{Code: http.StatusGatewayTimeout, Message: "timeout"}, want: true},
		{name: "403 googleapi", err: &googleapi.Error{Code: http.StatusForbidden, Message: "forbidden"}, want: false},
		{name: "404 googleapi", err: &googleapi.Error{Code: http.StatusNotFound, Message: "not found"}, want: false},
		{name: "400 googleapi", err: &googleapi.Error{Code: http.StatusBadRequest, Message: "bad request"}, want: false},
		// String-based fallback for non-googleapi errors
		{name: "RESOURCE_EXHAUSTED string", err: errors.New("googleapi: RESOURCE_EXHAUSTED"), want: true},
		{name: "Resource exhausted string", err: errors.New("Resource exhausted: too many requests"), want: true},
		{name: "rate limit string", err: errors.New("rate limit exceeded"), want: true},
		{name: "Overloaded string", err: errors.New("model Overloaded, try again"), want: true},
		{name: "quota exceeded string", err: errors.New("quota exceeded for project"), want: true},
		{name: "Internal error string", err: errors.New("Internal error occurred"), want: true},
		{name: "server error string", err: errors.New("server error: please retry"), want: true},
		{name: "permission denied string", err: errors.New("permission denied: insufficient access"), want: false},
		{name: "not found string", err: errors.New("model not found"), want: false},
		{name: "invalid argument string", err: errors.New("invalid argument: bad request"), want: false},
		{name: "auth error string", err: errors.New("authentication failed"), want: false},
		{name: "UNAVAILABLE string", err: errors.New("Error 503, Message: Visibility check was unavailable. Please retry the request, Status: UNAVAILABLE, Details: []"), want: true},
		{name: "unavailable lowercase string", err: errors.New("service unavailable: please retry"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRetryableVertexError(tt.err); got != tt.want {
				t.Errorf("isRetryableVertexError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryableVertexError_WrappedError(t *testing.T) {
	t.Parallel()

	t.Run("wrapped googleapi error", func(t *testing.T) {
		t.Parallel()
		original := &googleapi.Error{Code: 499, Message: "The operation was cancelled."}
		wrapped := fmt.Errorf("failed to send tool responses: %w", original)

		if !isRetryableVertexError(wrapped) {
			t.Error("isRetryableVertexError() = false, want true for wrapped 499 googleapi.Error")
		}
	})

	t.Run("wrapped ResourceExhausted string", func(t *testing.T) {
		t.Parallel()
		original := errors.New("rpc error: code = ResourceExhausted desc = 429")
		wrapped := fmt.Errorf("send_initial_message failed after 5 retries: %w", original)

		if !isRetryableVertexError(wrapped) {
			t.Error("isRetryableVertexError() = false, want true for wrapped ResourceExhausted error")
		}
	})
}
