/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestNewParams tests the NewParams function
func TestNewParams(t *testing.T) {
	tests := []struct {
		name          string
		input         json.RawMessage
		expectError   bool
		expectedError string
	}{{
		name:        "valid JSON object",
		input:       json.RawMessage(`{"key": "value", "number": 42}`),
		expectError: false,
	}, {
		name:        "empty JSON object",
		input:       json.RawMessage(`{}`),
		expectError: false,
	}, {
		name:          "invalid JSON",
		input:         json.RawMessage(`{invalid json`),
		expectError:   true,
		expectedError: "Failed to parse tool input:",
	}, {
		name:        "null input",
		input:       json.RawMessage(`null`),
		expectError: false, // json.Unmarshal of null to map[string]interface{} succeeds with nil map
	}, {
		name:          "array instead of object",
		input:         json.RawMessage(`["not", "an", "object"]`),
		expectError:   true,
		expectedError: "Failed to parse tool input:",
	}, {
		name:        "nested JSON object",
		input:       json.RawMessage(`{"outer": {"inner": "value"}, "array": [1, 2, 3]}`),
		expectError: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolUse := anthropic.ToolUseBlock{
				ID:    "test-id",
				Name:  "test-tool",
				Input: tt.input,
			}

			params, errResp := NewParams(toolUse)

			if tt.expectError {
				if errResp == nil {
					t.Errorf("NewParams() expected error but got none")
					return
				}
				if errMsg, ok := errResp["error"].(string); !ok {
					t.Errorf("NewParams() error response missing 'error' field")
				} else if len(tt.expectedError) > 0 && len(errMsg) < len(tt.expectedError) {
					t.Errorf("NewParams() error = %v, want prefix %v", errMsg, tt.expectedError)
				}
			} else {
				if errResp != nil {
					t.Errorf("NewParams() unexpected error: %v", errResp)
				}
				if params == nil {
					t.Errorf("NewParams() returned nil params")
				}
			}
		})
	}
}

// TestParams_Get tests the Get method
func TestParams_Get(t *testing.T) {
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{
			"string": "hello",
			"number": 42,
			"float": 3.14,
			"bool": true,
			"null": null,
			"object": {"nested": "value"},
			"array": [1, 2, 3]
		}`),
	}

	params, _ := NewParams(toolUse)

	tests := []struct {
		name      string
		paramName string
		wantValue any
		wantOk    bool
	}{
		{"existing string", "string", "hello", true},
		{"existing number", "number", float64(42), true},
		{"existing float", "float", float64(3.14), true},
		{"existing bool", "bool", true, true},
		{"existing null", "null", nil, true},
		{"existing object", "object", map[string]any{"nested": "value"}, true},
		{"existing array", "array", []any{float64(1), float64(2), float64(3)}, true},
		{"non-existing key", "missing", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotValue, gotOk := params.Get(tt.paramName)
			if gotOk != tt.wantOk {
				t.Errorf("Get() ok = %v, want %v", gotOk, tt.wantOk)
			}
			if !reflect.DeepEqual(gotValue, tt.wantValue) {
				t.Errorf("Get() value = %v (type %T), want %v (type %T)",
					gotValue, gotValue, tt.wantValue, tt.wantValue)
			}
		})
	}
}

// TestParam tests the Param function
func TestParam(t *testing.T) {
	t.Run("string parameters", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"name": "test", "empty": ""}`),
		}
		params, _ := NewParams(toolUse)

		// Test existing string
		got, errResp := Param[string](params, "name")
		if errResp != nil {
			t.Errorf("Param() unexpected error: %v", errResp)
		}
		if got != "test" {
			t.Errorf("Param() = %v, want %v", got, "test")
		}

		// Test empty string
		got, errResp = Param[string](params, "empty")
		if errResp != nil {
			t.Errorf("Param() unexpected error: %v", errResp)
		}
		if got != "" {
			t.Errorf("Param() = %v, want empty string", got)
		}

		// Test missing parameter
		_, errResp = Param[string](params, "missing")
		if errResp == nil {
			t.Errorf("Param() expected error for missing parameter")
		} else if errResp["error"] != "missing parameter is required" {
			t.Errorf("Param() error = %v, want 'missing parameter is required'", errResp["error"])
		}
	})

	t.Run("numeric parameters", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"int": 42, "float": 3.14, "zero": 0}`),
		}
		params, _ := NewParams(toolUse)

		// Test int conversion from float64
		gotInt, errResp := Param[int](params, "int")
		if errResp != nil {
			t.Errorf("Param[int]() unexpected error: %v", errResp)
		}
		if gotInt != 42 {
			t.Errorf("Param[int]() = %v, want %v", gotInt, 42)
		}

		// Test int32 conversion
		gotInt32, errResp := Param[int32](params, "int")
		if errResp != nil {
			t.Errorf("Param[int32]() unexpected error: %v", errResp)
		}
		if gotInt32 != 42 {
			t.Errorf("Param[int32]() = %v, want %v", gotInt32, 42)
		}

		// Test int64 conversion
		gotInt64, errResp := Param[int64](params, "int")
		if errResp != nil {
			t.Errorf("Param[int64]() unexpected error: %v", errResp)
		}
		if gotInt64 != 42 {
			t.Errorf("Param[int64]() = %v, want %v", gotInt64, 42)
		}

		// Test float64 (no conversion needed)
		gotFloat, errResp := Param[float64](params, "float")
		if errResp != nil {
			t.Errorf("Param[float64]() unexpected error: %v", errResp)
		}
		if gotFloat != 3.14 {
			t.Errorf("Param[float64]() = %v, want %v", gotFloat, 3.14)
		}
	})

	t.Run("type mismatches", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"string": "hello", "number": 42, "bool": true}`),
		}
		params, _ := NewParams(toolUse)

		// String where int expected
		_, errResp := Param[int](params, "string")
		if errResp == nil {
			t.Errorf("Param[int]() expected error for type mismatch")
		}

		// Number where bool expected
		_, errResp = Param[bool](params, "number")
		if errResp == nil {
			t.Errorf("Param[bool]() expected error for type mismatch")
		}

		// Bool where string expected
		_, errResp = Param[string](params, "bool")
		if errResp == nil {
			t.Errorf("Param[string]() expected error for type mismatch")
		}
	})

	t.Run("complex types", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{
				"slice": [1, 2, 3],
				"map": {"key": "value"}
			}`),
		}
		params, _ := NewParams(toolUse)

		// Test slice
		gotSlice, errResp := Param[[]any](params, "slice")
		if errResp != nil {
			t.Errorf("Param[[]interface{}]() unexpected error: %v", errResp)
		}
		if len(gotSlice) != 3 {
			t.Errorf("Param[[]interface{}]() slice length = %v, want 3", len(gotSlice))
		}

		// Test map
		gotMap, errResp := Param[map[string]any](params, "map")
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
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{}`),
		}
		params, _ := NewParams(toolUse)

		// String default
		gotStr, errResp := OptionalParam(params, "missing", "default")
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotStr != "default" {
			t.Errorf("OptionalParam() = %v, want 'default'", gotStr)
		}

		// Int default
		gotInt, errResp := OptionalParam(params, "missing", 99)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotInt != 99 {
			t.Errorf("OptionalParam() = %v, want 99", gotInt)
		}

		// Bool default
		gotBool, errResp := OptionalParam(params, "missing", true)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotBool != true {
			t.Errorf("OptionalParam() = %v, want true", gotBool)
		}
	})

	t.Run("existing parameters override default", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"name": "actual", "count": 42, "enabled": false}`),
		}
		params, _ := NewParams(toolUse)

		// String with default
		got, errResp := OptionalParam(params, "name", "default")
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if got != "actual" {
			t.Errorf("OptionalParam() = %v, want 'actual'", got)
		}

		// Int with default (converted from float64)
		gotInt, errResp := OptionalParam(params, "count", 0)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotInt != 42 {
			t.Errorf("OptionalParam() = %v, want 42", gotInt)
		}

		// Bool with default
		gotBool, errResp := OptionalParam(params, "enabled", true)
		if errResp != nil {
			t.Errorf("OptionalParam() unexpected error: %v", errResp)
		}
		if gotBool != false {
			t.Errorf("OptionalParam() = %v, want false", gotBool)
		}
	})

	t.Run("type mismatch returns error", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"wrongType": "string"}`),
		}
		params, _ := NewParams(toolUse)

		// Expecting int but got string
		_, errResp := OptionalParam(params, "wrongType", 0)
		if errResp == nil {
			t.Errorf("OptionalParam() expected error for type mismatch")
		} else if _, ok := errResp["error"].(string); !ok {
			t.Errorf("OptionalParam() error response missing 'error' field")
		}
	})

	t.Run("numeric conversions", func(t *testing.T) {
		toolUse := anthropic.ToolUseBlock{
			Input: json.RawMessage(`{"num": 123.0}`),
		}
		params, _ := NewParams(toolUse)

		// int32 conversion
		got32, errResp := OptionalParam[int32](params, "num", 0)
		if errResp != nil {
			t.Errorf("OptionalParam[int32]() unexpected error: %v", errResp)
		}
		if got32 != 123 {
			t.Errorf("OptionalParam[int32]() = %v, want 123", got32)
		}

		// int64 conversion
		got64, errResp := OptionalParam[int64](params, "num", 0)
		if errResp != nil {
			t.Errorf("OptionalParam[int64]() unexpected error: %v", errResp)
		}
		if got64 != 123 {
			t.Errorf("OptionalParam[int64]() = %v, want 123", got64)
		}
	})
}

// TestParamsConcurrent tests thread safety
func TestParamsConcurrent(t *testing.T) {
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{"shared": "value", "number": 42}`),
	}
	params, _ := NewParams(toolUse)

	done := make(chan bool)

	// Launch multiple goroutines accessing the same params
	for range 10 {
		go func() {
			// Test Get
			val, ok := params.Get("shared")
			if !ok || val != "value" {
				t.Errorf("Concurrent Get() failed")
			}

			// Test Param
			str, err := Param[string](params, "shared")
			if err != nil || str != "value" {
				t.Errorf("Concurrent Param() failed")
			}

			// Test OptionalParam
			num, err := OptionalParam(params, "number", 0)
			if err != nil || num != 42 {
				t.Errorf("Concurrent OptionalParam() failed")
			}

			done <- true
		}()
	}

	// Wait for all goroutines
	for range 10 {
		<-done
	}
}
