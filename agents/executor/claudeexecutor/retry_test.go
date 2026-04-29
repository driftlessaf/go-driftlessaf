/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestIsRetryableClaudeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Structured API errors (*anthropic.Error)
		{name: "nil error", err: nil, want: false},
		{name: "non-API error", err: fmt.Errorf("connection refused"), want: false},
		{name: "429 rate limit", err: &anthropic.Error{StatusCode: 429}, want: true},
		{name: "503 unavailable", err: &anthropic.Error{StatusCode: 503}, want: true},
		{name: "504 gateway timeout", err: &anthropic.Error{StatusCode: 504}, want: true},
		{name: "529 overloaded", err: &anthropic.Error{StatusCode: 529}, want: true},
		{name: "400 bad request", err: &anthropic.Error{StatusCode: 400}, want: false},
		{name: "401 unauthorized", err: &anthropic.Error{StatusCode: 401}, want: false},
		{name: "403 forbidden", err: &anthropic.Error{StatusCode: 403}, want: false},
		{name: "404 not found", err: &anthropic.Error{StatusCode: 404}, want: false},
		{name: "500 internal error", err: &anthropic.Error{StatusCode: 500}, want: false},
		// SSE streaming errors (plain fmt.Errorf with raw JSON from ssestream package)
		{name: "streaming overloaded_error", err: fmt.Errorf(`received error while streaming: {"type":"error","error":{"details":null,"type":"overloaded_error","message":"Overloaded"},"request_id":"req_vrtx_011CYejFMV3t43MQ1E377Xn9"}`), want: true},
		{name: "streaming rate_limit_error", err: fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`), want: true},
		{name: "streaming api_error", err: fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`), want: true},
		{name: "streaming invalid_request", err: fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"invalid_request_error","message":"Bad input"}}`), want: false},
		{name: "streaming authentication_error", err: fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"authentication_error","message":"Invalid key"}}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRetryableClaudeError(tt.err); got != tt.want {
				t.Errorf("isRetryableClaudeError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestResponseCodeFromError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil error maps to 0", err: nil, want: 0},
		// Structured API errors copy StatusCode verbatim.
		{name: "429 anthropic.Error", err: &anthropic.Error{StatusCode: 429}, want: 429},
		{name: "500 anthropic.Error", err: &anthropic.Error{StatusCode: 500}, want: 500},
		{name: "529 anthropic.Error", err: &anthropic.Error{StatusCode: 529}, want: 529},
		{name: "wrapped 429", err: fmt.Errorf("stream: %w", &anthropic.Error{StatusCode: 429}), want: 429},
		// SSE streaming errors recover the code from the error_type string.
		{name: "streaming rate_limit_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error"}}`), want: 429},
		{name: "streaming overloaded_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"overloaded_error"}}`), want: 529},
		{name: "streaming api_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"api_error"}}`), want: 500},
		// Unrecognised errors map to -1, surfaced as "unknown" by responseCodeAttr.
		{name: "opaque error", err: errors.New("connection refused"), want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responseCodeFromError(tt.err); got != tt.want {
				t.Errorf("responseCodeFromError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestResponseCodeFromMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		want int
	}{
		{name: "rate_limit_error", s: `received error while streaming: {"type":"error","error":{"type":"rate_limit_error"}}`, want: 429},
		{name: "overloaded_error", s: `received error while streaming: {"type":"error","error":{"type":"overloaded_error"}}`, want: 529},
		{name: "api_error", s: `received error while streaming: {"type":"error","error":{"type":"api_error"}}`, want: 500},
		{name: "no match", s: "connection refused", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responseCodeFromMessage(tt.s); got != tt.want {
				t.Errorf("responseCodeFromMessage(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

func TestResponseCodeAttr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "0 is success", code: 0, want: "200"},
		{name: "negative is unknown", code: -1, want: "unknown"},
		{name: "429", code: 429, want: "429"},
		{name: "500", code: 500, want: "500"},
		{name: "529", code: 529, want: "529"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responseCodeAttr(tt.code); got != tt.want {
				t.Errorf("responseCodeAttr(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestIsRetryableClaudeError_WrappedError(t *testing.T) {
	t.Parallel()

	// Simulates the error wrapping from retry.RetryWithBackoff:
	// "stream_message failed after 5 retries: <original error>"
	original := &anthropic.Error{StatusCode: 429}
	wrapped := fmt.Errorf("stream_message failed after 5 retries: %w", original)

	if !isRetryableClaudeError(wrapped) {
		t.Error("isRetryableClaudeError() = false, want true for wrapped 429 error")
	}
}
