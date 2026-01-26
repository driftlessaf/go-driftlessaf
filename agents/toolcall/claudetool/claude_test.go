/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
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
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("Error() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestErrorWithContext tests the ErrorWithContext function
func TestErrorWithContext(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		context  map[string]any
		expected map[string]any
	}{{
		name:    "error with empty context",
		err:     errors.New("test error"),
		context: map[string]any{},
		expected: map[string]any{
			"error": "test error",
		},
	}, {
		name: "error with single context field",
		err:  errors.New("file not found"),
		context: map[string]any{
			"filename": "test.txt",
		},
		expected: map[string]any{
			"error":    "file not found",
			"filename": "test.txt",
		},
	}, {
		name: "error with multiple context fields",
		err:  errors.New("validation failed"),
		context: map[string]any{
			"field":  "email",
			"value":  "invalid@",
			"line":   42,
			"column": 10,
		},
		expected: map[string]any{
			"error":  "validation failed",
			"field":  "email",
			"value":  "invalid@",
			"line":   42,
			"column": 10,
		},
	}, {
		name:    "error with nil context",
		err:     errors.New("nil context test"),
		context: nil,
		expected: map[string]any{
			"error": "nil context test",
		},
	}, {
		name: "context error field overwrites error",
		err:  errors.New("actual error"),
		context: map[string]any{
			"error": "this overwrites the actual error",
			"other": "preserved",
		},
		expected: map[string]any{
			"error": "this overwrites the actual error", // Context fields overwrite the error field
			"other": "preserved",
		},
	}, {
		name: "complex context values",
		err:  errors.New("complex error"),
		context: map[string]any{
			"array": []int{1, 2, 3},
			"nested": map[string]any{
				"key": "value",
			},
			"bool":  true,
			"float": 3.14,
		},
		expected: map[string]any{
			"error": "complex error",
			"array": []int{1, 2, 3},
			"nested": map[string]any{
				"key": "value",
			},
			"bool":  true,
			"float": 3.14,
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ErrorWithContext(tt.err, tt.context)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("ErrorWithContext() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestErrorWithContext_ErrorNil tests behavior with nil error
func TestErrorWithContext_ErrorNil(t *testing.T) {
	// Test what happens when a nil error is passed
	defer func() {
		if r := recover(); r != nil {
			// If it panics, that's one valid behavior
			t.Logf("Function panicked with nil error: %v", r)
		}
	}()

	context := map[string]any{
		"key": "value",
	}

	// This might panic or return "<nil>" as the error message
	got := ErrorWithContext(nil, context)
	if errorMsg, ok := got["error"].(string); ok {
		if errorMsg != "<nil>" {
			t.Errorf("Expected error message to be '<nil>' for nil error, got %s", errorMsg)
		}
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

			// Test ErrorWithContext
			err := fmt.Errorf("error %d", id)
			ctx := map[string]any{"id": id}
			result2 := ErrorWithContext(err, ctx)
			if result2["error"] != err.Error() || result2["id"] != id {
				t.Errorf("Concurrent ErrorWithContext failed for id %d", id)
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for range 10 {
		<-done
	}
}
