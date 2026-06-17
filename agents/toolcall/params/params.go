/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package params

import (
	"fmt"
	"maps"
	"reflect"
)

// Extract extracts a required parameter from args with type safety.
// Returns an error if the parameter is missing or cannot be converted to T.
func Extract[T any](args map[string]any, name string) (T, error) {
	var zero T

	value, exists := args[name]
	if !exists {
		return zero, fmt.Errorf("%s parameter is required", name)
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	if v, ok := convertNumeric[T](value); ok {
		return v, nil
	}

	return zero, typeMismatchError[T](name, value)
}

// ExtractOptional extracts an optional parameter with a default value.
// Returns the default if the parameter doesn't exist, or an error if type conversion fails.
func ExtractOptional[T any](args map[string]any, name string, defaultValue T) (T, error) {
	value, exists := args[name]
	if !exists {
		return defaultValue, nil
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	if v, ok := convertNumeric[T](value); ok {
		return v, nil
	}

	var zero T
	return zero, typeMismatchError[T](name, value)
}

// convertNumeric handles common JSON numeric conversions (float64 -> int/int32/int64).
func convertNumeric[T any](value any) (T, bool) {
	var zero T
	switch any(zero).(type) {
	case int:
		if floatVal, ok := value.(float64); ok {
			return any(int(floatVal)).(T), true
		}
	case int32:
		if floatVal, ok := value.(float64); ok {
			return any(int32(floatVal)).(T), true
		}
	case int64:
		if floatVal, ok := value.(float64); ok {
			return any(int64(floatVal)).(T), true
		}
	}
	return zero, false
}

// typeMismatchError builds a parameter type error phrased in JSON terms rather
// than Go types, so the message is actionable for an LLM caller (e.g. "must be a
// JSON object, got string" instead of "must be of type map[string]interface {}").
// When an object is expected but a string was supplied, it adds a hint for the
// common mistake of JSON-encoding the object into a string instead of passing it
// directly as a nested object.
func typeMismatchError[T any](name string, value any) error {
	want := jsonTypeName(reflect.TypeFor[T]())
	got := jsonTypeName(reflect.TypeOf(value))
	if want == "object" && got == "string" {
		return fmt.Errorf("%s parameter must be a JSON %s, got %s — it looks like you JSON-encoded the object into a string; pass it directly as a nested JSON object, not as a string", name, want, got)
	}
	return fmt.Errorf("%s parameter must be a JSON %s, got %s", name, want, got)
}

// jsonTypeName maps a Go type to the JSON type name an LLM caller reasons about.
// A nil type (untyped JSON null) reports "null".
func jsonTypeName(t reflect.Type) string {
	if t == nil {
		return "null"
	}
	switch t.Kind() {
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Pointer:
		return jsonTypeName(t.Elem())
	default:
		return t.Kind().String()
	}
}

// Error creates an error response map.
func Error(format string, args ...any) map[string]any {
	return map[string]any{
		"error": fmt.Sprintf(format, args...),
	}
}

// ErrorWithContext creates an error response with additional context fields.
func ErrorWithContext(err error, context map[string]any) map[string]any {
	response := map[string]any{
		"error": err.Error(),
	}
	maps.Copy(response, context)
	return response
}
