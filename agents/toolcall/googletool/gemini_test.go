/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genai"
)

// TestError tests the Error function
func TestError(t *testing.T) {
	tests := []struct {
		name          string
		callID        string
		callName      string
		format        string
		args          []any
		expectedError string
	}{{
		name:          "simple error message",
		callID:        "id-1",
		callName:      "func-1",
		format:        "simple error",
		args:          nil,
		expectedError: "simple error",
	}, {
		name:          "formatted error with string",
		callID:        "id-2",
		callName:      "func-2",
		format:        "error: %s",
		args:          []any{"test message"},
		expectedError: "error: test message",
	}, {
		name:          "formatted error with multiple args",
		callID:        "id-3",
		callName:      "func-3",
		format:        "error %d: %s at line %d",
		args:          []any{404, "not found", 42},
		expectedError: "error 404: not found at line 42",
	}, {
		name:          "empty format string",
		callID:        "id-4",
		callName:      "func-4",
		format:        "",
		args:          nil,
		expectedError: "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := &genai.FunctionCall{
				ID:   tt.callID,
				Name: tt.callName,
			}

			got := Error(call, tt.format, tt.args...)

			if got.ID != tt.callID {
				t.Errorf("Error() ID: got = %v, want = %v", got.ID, tt.callID)
			}
			if got.Name != tt.callName {
				t.Errorf("Error() Name: got = %v, want = %v", got.Name, tt.callName)
			}
			if resp := got.Response; resp["error"] != tt.expectedError {
				t.Errorf("Error() error: got = %v, want = %v", resp["error"], tt.expectedError)
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
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := &genai.FunctionCall{
				ID:   "test-id",
				Name: "test-func",
			}

			got := ErrorWithContext(call, tt.err, tt.context)

			if got.ID != call.ID || got.Name != call.Name {
				t.Errorf("ErrorWithContext() wrong ID/Name")
			}
			if diff := cmp.Diff(tt.expected, got.Response); diff != "" {
				t.Errorf("ErrorWithContext() Response mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

// TestErrorWithContext_ErrorNil tests behavior with nil error
func TestErrorWithContext_ErrorNil(t *testing.T) {
	call := &genai.FunctionCall{
		ID:   "test-id",
		Name: "test-func",
	}
	context := map[string]any{
		"key": "value",
	}

	// Test what happens when a nil error is passed
	defer func() {
		if r := recover(); r != nil {
			// If it panics, that's one valid behavior
			t.Logf("Function panicked with nil error: %v", r)
		}
	}()

	// This might panic or return "<nil>" as the error message
	got := ErrorWithContext(call, nil, context)
	if resp := got.Response; resp != nil {
		if errorMsg, ok := resp["error"].(string); ok {
			if errorMsg != "<nil>" {
				t.Errorf("error message: got = %s, want = <nil>", errorMsg)
			}
		}
	}
}

// TestConcurrent tests thread safety
func TestConcurrent(t *testing.T) {
	call := &genai.FunctionCall{
		ID:   "concurrent-id",
		Name: "concurrent-func",
	}

	done := make(chan bool)

	// Launch multiple goroutines
	for i := range 10 {
		go func(id int) {
			// Test Error
			resp := Error(call, "error %d", id)
			if resp.ID != call.ID || resp.Name != call.Name {
				t.Errorf("Concurrent Error() failed")
			}

			// Test ErrorWithContext
			resp = ErrorWithContext(call, errors.New("boom"), map[string]any{"id": id})
			if resp.Response["error"] != "boom" || resp.Response["id"] != id {
				t.Errorf("Concurrent ErrorWithContext() failed for id %d", id)
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}
}
