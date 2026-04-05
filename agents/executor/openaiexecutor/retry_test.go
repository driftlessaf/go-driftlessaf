/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor

import (
	"fmt"
	"testing"

	"github.com/openai/openai-go"
)

func TestIsRetryableOpenAIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{{
		name: "nil error",
		err:  nil,
		want: false,
	}, {
		name: "non-API error",
		err:  fmt.Errorf("connection refused"),
		want: false,
	}, {
		name: "429 rate limit",
		err:  &openai.Error{StatusCode: 429},
		want: true,
	}, {
		name: "500 internal server error",
		err:  &openai.Error{StatusCode: 500},
		want: true,
	}, {
		name: "502 bad gateway",
		err:  &openai.Error{StatusCode: 502},
		want: true,
	}, {
		name: "503 unavailable",
		err:  &openai.Error{StatusCode: 503},
		want: true,
	}, {
		name: "504 gateway timeout",
		err:  &openai.Error{StatusCode: 504},
		want: true,
	}, {
		name: "400 bad request",
		err:  &openai.Error{StatusCode: 400},
		want: false,
	}, {
		name: "401 unauthorized",
		err:  &openai.Error{StatusCode: 401},
		want: false,
	}, {
		name: "403 forbidden",
		err:  &openai.Error{StatusCode: 403},
		want: false,
	}, {
		name: "404 not found",
		err:  &openai.Error{StatusCode: 404},
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isRetryableOpenAIError(tt.err); got != tt.want {
				t.Errorf("isRetryableOpenAIError(%v): got = %v, wanted = %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryableOpenAIError_WrappedError(t *testing.T) {
	t.Parallel()

	original := &openai.Error{StatusCode: 429}
	wrapped := fmt.Errorf("chat_completion failed after 5 retries: %w", original)

	if !isRetryableOpenAIError(wrapped) {
		t.Error("isRetryableOpenAIError(): got = false, wanted = true for wrapped 429 error")
	}
}
