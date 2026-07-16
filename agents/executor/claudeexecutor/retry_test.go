/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// newAPIError builds an *anthropic.Error with a populated body error type the
// way the SDK does: the errorType field is unexported and only set by
// UnmarshalJSON, so the {"error":{"type":...}} envelope is unmarshalled first
// and StatusCode is stamped afterwards, mirroring how the SDK copies it from
// the HTTP response. Request/Response are populated because Error()
// dereferences both.
func newAPIError(t *testing.T, statusCode int, errType string) *anthropic.Error {
	t.Helper()
	apiErr := &anthropic.Error{}
	body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"boom"}}`, errType)
	if err := json.Unmarshal([]byte(body), apiErr); err != nil {
		t.Fatalf("json.Unmarshal(%s) = %v", body, err)
	}
	apiErr.StatusCode = statusCode
	apiErr.Request = &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "api.anthropic.com", Path: "/v1/messages"}}
	apiErr.Response = &http.Response{StatusCode: statusCode}
	return apiErr
}

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
		// In-stream errors: *anthropic.Error from an SSE error event on an
		// already-open stream, so StatusCode carries the 200 of the stream
		// open and the failure lives in the body error type.
		{name: "in-stream overloaded_error", err: newAPIError(t, 200, "overloaded_error"), want: true},
		{name: "in-stream rate_limit_error", err: newAPIError(t, 200, "rate_limit_error"), want: true},
		{name: "in-stream api_error", err: newAPIError(t, 200, "api_error"), want: true},
		{name: "unset status with overloaded_error body", err: newAPIError(t, 0, "overloaded_error"), want: true},
		{name: "in-stream invalid_request_error", err: newAPIError(t, 200, "invalid_request_error"), want: false},
		{name: "200 with empty error type", err: &anthropic.Error{StatusCode: 200}, want: false},
		// A real (non-200) HTTP status stays authoritative: the body type
		// must not make a transport-level 500 retryable.
		{name: "500 with api_error body", err: newAPIError(t, 500, "api_error"), want: false},
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
		// In-stream errors (StatusCode 200 from the successful stream open)
		// recover the code from the body error type instead of the transport
		// status, so metrics no longer label them response_code="200".
		{name: "in-stream overloaded_error", err: newAPIError(t, 200, "overloaded_error"), want: 529},
		{name: "in-stream rate_limit_error", err: newAPIError(t, 200, "rate_limit_error"), want: 429},
		{name: "in-stream api_error", err: newAPIError(t, 200, "api_error"), want: 500},
		{name: "wrapped in-stream overloaded_error", err: fmt.Errorf("stream: %w", newAPIError(t, 200, "overloaded_error")), want: 529},
		{name: "unset status with overloaded_error body", err: newAPIError(t, 0, "overloaded_error"), want: 529},
		// Unrecognised or empty body types keep the StatusCode verbatim
		// rather than inventing a code.
		{name: "in-stream invalid_request_error", err: newAPIError(t, 200, "invalid_request_error"), want: 200},
		{name: "200 with empty error type", err: &anthropic.Error{StatusCode: 200}, want: 200},
		// A real (non-200) HTTP status is authoritative over the body type.
		{name: "500 with api_error body", err: newAPIError(t, 500, "api_error"), want: 500},
		// SSE streaming errors recover the code from the error_type string.
		{name: "streaming rate_limit_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error"}}`), want: 429},
		{name: "streaming overloaded_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"overloaded_error"}}`), want: 529},
		{name: "streaming api_error", err: errors.New(`received error while streaming: {"type":"error","error":{"type":"api_error"}}`), want: 500},
		// Unrecognised errors map to -1, surfaced as "unknown" by the telemetry recorder.
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
