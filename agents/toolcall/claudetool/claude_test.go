/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
)

// TestReasoningParameterDoesNotAskForReasoning guards the wording of the
// reasoning parameter injected into every tool. Claude's reasoning_extraction
// safety classifier can refuse a turn of a tool loop whose tools ask the
// model to put its reasoning in a tool input, so the description may ask
// what the call is for, never for why or how the model thought.
func TestReasoningParameterDoesNotAskForReasoning(t *testing.T) {
	param := toolParam(toolcall.Definition{
		Name:       "lookup",
		Parameters: []toolcall.Parameter{{Name: "key", Type: "string", Required: true}},
	})
	props, ok := param.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("properties: got = %T, want = map[string]any", param.InputSchema.Properties)
	}
	reasoning, ok := props["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning parameter: got = %v, want = a schema", props["reasoning"])
	}
	description, _ := reasoning["description"].(string)
	if description == "" {
		t.Fatal("reasoning description: got = empty, want = a request for a short note")
	}
	for _, phrase := range []string{"why", "reasoning", "thinking", "thought", "step by step"} {
		if strings.Contains(strings.ToLower(description), phrase) {
			t.Errorf("reasoning description %q contains %q: it must ask for a short note on the call, not for the model's reasoning", description, phrase)
		}
	}
}

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
