/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestError tests the Error function with various input scenarios
func TestError(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		args     []any
		expected map[string]any
	}{{
		name:   "simple error message",
		format: "simple error",
		args:   nil,
		expected: map[string]any{
			"error": "simple error",
		},
	}, {
		name:   "formatted error with string",
		format: "error: %s",
		args:   []any{"test message"},
		expected: map[string]any{
			"error": "error: test message",
		},
	}, {
		name:   "formatted error with multiple args",
		format: "error %d: %s at line %d",
		args:   []any{404, "not found", 42},
		expected: map[string]any{
			"error": "error 404: not found at line 42",
		},
	}, {
		name:   "empty format string",
		format: "",
		args:   nil,
		expected: map[string]any{
			"error": "",
		},
	}, {
		name:   "format with no args but placeholders",
		format: "error: %s %d",
		args:   nil,
		expected: map[string]any{
			"error": "error: %!s(MISSING) %!d(MISSING)",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Error(tt.format, tt.args...)
			if diff := cmp.Diff(tt.expected, got); diff != "" {
				t.Errorf("Error() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestErrorConcurrent tests thread safety of the functions
func TestErrorConcurrent(t *testing.T) {
	// Test concurrent access to ensure thread safety
	done := make(chan bool)

	for i := range 10 {
		go func(id int) {
			// Test Error
			result := Error("concurrent error %d", id)
			expected := fmt.Sprintf("concurrent error %d", id)
			if result["error"] != expected {
				t.Errorf("Concurrent Error failed: got %v, want %v", result["error"], expected)
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for range 10 {
		<-done
	}
}
