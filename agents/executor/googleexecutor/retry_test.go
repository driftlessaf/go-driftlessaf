/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"errors"
	"testing"
)

func TestIsRetryableVertexError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "429 status", err: errors.New("rpc error: code = ResourceExhausted desc = 429"), want: true},
		{name: "RESOURCE_EXHAUSTED", err: errors.New("googleapi: RESOURCE_EXHAUSTED"), want: true},
		{name: "Resource exhausted", err: errors.New("Resource exhausted: too many requests"), want: true},
		{name: "rate limit", err: errors.New("rate limit exceeded"), want: true},
		{name: "Overloaded", err: errors.New("model Overloaded, try again"), want: true},
		{name: "503 status", err: errors.New("503 Service Unavailable"), want: true},
		{name: "quota exceeded", err: errors.New("quota exceeded for project"), want: true},
		{name: "Internal error", err: errors.New("Internal error occurred"), want: true},
		{name: "server error", err: errors.New("server error: please retry"), want: true},
		{name: "permission denied", err: errors.New("permission denied: insufficient access"), want: false},
		{name: "not found", err: errors.New("model not found"), want: false},
		{name: "invalid argument", err: errors.New("invalid argument: bad request"), want: false},
		{name: "auth error", err: errors.New("authentication failed"), want: false},
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
