/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool

import (
	"errors"
	"reflect"
	"testing"

	"google.golang.org/genai"
)

// TestParam tests the Param function
func TestParam(t *testing.T) {
	t.Run("string parameters", func(t *testing.T) {
		call := &genai.FunctionCall{
			ID:   "test-id",
			Name: "test-function",
			Args: map[string]any{
				"name":  "test-value",
				"empty": "",
			},
		}

		// Test existing string
		got, errResp := Param[string](call, "name")
		if errResp != nil {
			t.Errorf("Param() unexpected error: %v", errResp)
		}
		if got != "test-value" {
			t.Errorf("Param() = %v, want %v", got, "test-value")
		}

		// Test empty string
		got, errResp = Param[string](call, "empty")
		if errResp != nil {
			t.Errorf("Param() unexpected error: %v", errResp)
		}
		if got != "" {
			t.Errorf("Param() = %v, want empty string", got)
		}

		// Test missing parameter
		_, errResp = Param[string](call, "missing")
		if errResp == nil {
			t.Errorf("Param() expected error for missing parameter")
		} else {
			if errResp.ID != call.ID || errResp.Name != call.Name {
				t.Errorf("Param() error response has wrong ID/Name")
			}
			if resp := errResp.Response; resp["error"] != "missing parameter is required" {
				t.Errorf("Param() error = %v, want 'missing parameter is required'", resp["error"])
			}
		}
	})

	t.Run("numeric parameters", func(t *testing.T) {
		call := &genai.FunctionCall{
			ID:   "test-id",
			Name: "test-function",
			Args: map[string]any{
				"int":   float64(42), // JSON numbers come as float64
				"float": 3.14,
				"zero":  float64(0),
			},
		}

		// Test int conversion from float64
		gotInt, errResp := Param[int](call, "int")
		if errResp != nil {
			t.Errorf("Param[int]() unexpected error: %v", errResp)
		}
		if gotInt != 42 {
			t.Errorf("Param[int]() = %v, want %v", gotInt, 42)
		}

		// Test int32 conversion
		gotInt32, errResp := Param[int32](call, "int")
		if errResp != nil {
			t.Errorf("Param[int32]() unexpected error: %v", errResp)
		}
		if gotInt32 != 42 {
			t.Errorf("Param[int32]() = %v, want %v", gotInt32, 42)
		}

		// Test int64 conversion
		gotInt64, errResp := Param[int64](call, "int")
		if errResp != nil {
			t.Errorf("Param[int64]() unexpected error: %v", errResp)
		}
		if gotInt64 != 42 {
			t.Errorf("Param[int64]() = %v, want %v", gotInt64, 42)
		}

		// Test float64 (no conversion needed)
		gotFloat, errResp := Param[float64](call, "float")
		if errResp != nil {
			t.Errorf("Param[float64]() unexpected error: %v", errResp)
		}
		if gotFloat != 3.14 {
			t.Errorf("Param[float64]() = %v, want %v", gotFloat, 3.14)
		}

		// Test zero value
		gotZero, errResp := Param[int](call, "zero")
		if errResp != nil {
			t.Errorf("Param[int]() unexpected error: %v", errResp)
		}
		if gotZero != 0 {
			t.Errorf("Param[int]() = %v, want %v", gotZero, 0)
		}
	})

	t.Run("boolean parameters", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{
				"true":  true,
				"false": false,
			},
		}

		got, errResp := Param[bool](call, "true")
		if errResp != nil {
			t.Errorf("Param[bool]() unexpected error: %v", errResp)
		}
		if got != true {
			t.Errorf("Param[bool]() = %v, want true", got)
		}

		got, errResp = Param[bool](call, "false")
		if errResp != nil {
			t.Errorf("Param[bool]() unexpected error: %v", errResp)
		}
		if got != false {
			t.Errorf("Param[bool]() = %v, want false", got)
		}
	})

	t.Run("type mismatches", func(t *testing.T) {
		call := &genai.FunctionCall{
			ID:   "test-id",
			Name: "test-function",
			Args: map[string]any{
				"string": "hello",
				"number": float64(42),
				"bool":   true,
			},
		}

		// String where int expected
		_, errResp := Param[int](call, "string")
		if errResp == nil {
			t.Errorf("Param[int]() expected error for type mismatch")
		} else if resp := errResp.Response; resp != nil {
			if _, hasError := resp["error"].(string); !hasError {
				t.Errorf("Param[int]() error response missing error message")
			}
		}

		// Number where bool expected
		_, errResp = Param[bool](call, "number")
		if errResp == nil {
			t.Errorf("Param[bool]() expected error for type mismatch")
		}

		// Bool where string expected
		_, errResp = Param[string](call, "bool")
		if errResp == nil {
			t.Errorf("Param[string]() expected error for type mismatch")
		}
	})

	t.Run("complex types", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{
				"slice": []any{1, 2, 3},
				"map":   map[string]any{"key": "value"},
			},
		}

		// Test slice
		gotSlice, errResp := Param[[]any](call, "slice")
		if errResp != nil {
			t.Errorf("Param[[]any]() unexpected error: %v", errResp)
		}
		if len(gotSlice) != 3 {
			t.Errorf("Param[[]any]() slice length = %v, want 3", len(gotSlice))
		}

		// Test map
		gotMap, errResp := Param[map[string]any](call, "map")
		if errResp != nil {
			t.Errorf("Param[map]() unexpected error: %v", errResp)
		}
		if gotMap["key"] != "value" {
			t.Errorf("Param[map]() map['key'] = %v, want 'value'", gotMap["key"])
		}
	})
}

// TestOptionalParam tests the OptionalParam function
func TestOptionalParam(t *testing.T) {
	t.Run("missing parameters return default", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{},
		}

		// String default
		gotStr, errResp := OptionalParam(call, "missing", "default")
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotStr != "default" {
			t.Errorf("OptionalParam() = %v, want 'default'", gotStr)
		}

		// Int default
		gotInt, errResp := OptionalParam(call, "missing", 99)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotInt != 99 {
			t.Errorf("OptionalParam() = %v, want 99", gotInt)
		}

		// Bool default
		gotBool, errResp := OptionalParam(call, "missing", true)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotBool != true {
			t.Errorf("OptionalParam() = %v, want true", gotBool)
		}
	})

	t.Run("existing parameters override default", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{
				"name":    "actual",
				"count":   float64(42),
				"enabled": false,
			},
		}

		// String with default
		got, errResp := OptionalParam(call, "name", "default")
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if got != "actual" {
			t.Errorf("OptionalParam() = %v, want 'actual'", got)
		}

		// Int with default (converted from float64)
		gotInt, errResp := OptionalParam(call, "count", 0)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotInt != 42 {
			t.Errorf("OptionalParam() = %v, want 42", gotInt)
		}

		// Bool with default
		gotBool, errResp := OptionalParam(call, "enabled", true)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotBool != false {
			t.Errorf("OptionalParam() = %v, want false", gotBool)
		}
	})

	t.Run("type mismatch returns error", func(t *testing.T) {
		call := &genai.FunctionCall{
			ID:   "test-id",
			Name: "test-function",
			Args: map[string]any{
				"wrongType": "string",
			},
		}

		// Expecting int but got string
		_, errResp := OptionalParam(call, "wrongType", 0)
		if errResp == nil {
			t.Errorf("OptionalParam() expected error for type mismatch")
		} else {
			if errResp.ID != call.ID || errResp.Name != call.Name {
				t.Errorf("OptionalParam() error response has wrong ID/Name")
			}
			if resp := errResp.Response; resp != nil {
				if _, hasError := resp["error"].(string); !hasError {
					t.Errorf("OptionalParam() error response missing error message")
				}
			}
		}
	})

	t.Run("numeric conversions", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{
				"num": float64(123),
			},
		}

		// int32 conversion
		got32, errResp := OptionalParam[int32](call, "num", 0)
		if errResp != nil {
			t.Errorf("OptionalParam[int32]() unexpected error: %v", errResp)
		}
		if got32 != 123 {
			t.Errorf("OptionalParam[int32]() = %v, want 123", got32)
		}

		// int64 conversion
		got64, errResp := OptionalParam[int64](call, "num", 0)
		if errResp != nil {
			t.Errorf("OptionalParam[int64]() unexpected error: %v", errResp)
		}
		if got64 != 123 {
			t.Errorf("OptionalParam[int64]() = %v, want 123", got64)
		}
	})

	t.Run("nil values", func(t *testing.T) {
		call := &genai.FunctionCall{
			Args: map[string]any{
				"null": nil,
			},
		}

		// Nil should not match any type except interface{} or any
		_, errResp := OptionalParam[string](call, "null", "default")
		if errResp == nil {
			t.Errorf("OptionalParam[string]() expected error for nil value")
		}

		// any with nil value should fail type assertion too
		_, errResp = OptionalParam[any](call, "null", "default")
		if errResp == nil {
			t.Errorf("OptionalParam[any]() expected error for nil value")
		}
	})
}

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
				t.Errorf("Error() ID = %v, want %v", got.ID, tt.callID)
			}
			if got.Name != tt.callName {
				t.Errorf("Error() Name = %v, want %v", got.Name, tt.callName)
			}
			if resp := got.Response; resp["error"] != tt.expectedError {
				t.Errorf("Error() error = %v, want %v", resp["error"], tt.expectedError)
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
			if !reflect.DeepEqual(got.Response, tt.expected) {
				t.Errorf("ErrorWithContext() Response = %v, want %v", got.Response, tt.expected)
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
				t.Errorf("Expected error message to be '<nil>' for nil error, got %s", errorMsg)
			}
		}
	}
}

// TestConcurrent tests thread safety
func TestConcurrent(t *testing.T) {
	call := &genai.FunctionCall{
		ID:   "concurrent-id",
		Name: "concurrent-func",
		Args: map[string]any{
			"shared": "value",
			"number": float64(42),
		},
	}

	done := make(chan bool)

	// Launch multiple goroutines
	for i := range 10 {
		go func(id int) {
			// Test Param
			val, err := Param[string](call, "shared")
			if err != nil || val != "value" {
				t.Errorf("Concurrent Param() failed")
			}

			// Test OptionalParam
			num, err := OptionalParam(call, "number", float64(0))
			if err != nil || num != 42 {
				t.Errorf("Concurrent OptionalParam() failed")
			}

			// Test Error
			resp := Error(call, "error %d", id)
			if resp.ID != call.ID || resp.Name != call.Name {
				t.Errorf("Concurrent Error() failed")
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}
}
