/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
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
