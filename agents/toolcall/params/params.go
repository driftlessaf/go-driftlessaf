/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package params

import (
	"fmt"
	"maps"
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

	return zero, fmt.Errorf("%s parameter must be of type %T, got %T", name, zero, value)
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
	return zero, fmt.Errorf("%s parameter must be of type %T, got %T", name, zero, value)
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
