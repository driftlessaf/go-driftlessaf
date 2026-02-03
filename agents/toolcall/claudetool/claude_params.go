/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/anthropics/anthropic-sdk-go"
)

// Params provides efficient parameter extraction from Claude tool use blocks
type Params struct {
	inputMap map[string]any
}

// NewParams creates a new parameter extractor for Claude tool calls.
// It deserializes the input JSON once and returns an interface for accessing parameters.
func NewParams(toolUse anthropic.ToolUseBlock) (*Params, map[string]any) {
	var inputMap map[string]any
	if err := json.Unmarshal(toolUse.Input, &inputMap); err != nil {
		return nil, map[string]any{
			"error": fmt.Sprintf("Failed to parse tool input: %v", err),
		}
	}

	return &Params{
		inputMap: inputMap,
	}, nil
}

// Get returns the value for a given parameter name
func (cp *Params) Get(name string) (any, bool) {
	val, exists := cp.inputMap[name]
	return val, exists
}

// Param extracts a required parameter with type safety
func Param[T any](cp *Params, name string) (T, map[string]any) {
	var zero T

	value, exists := cp.inputMap[name]
	if !exists {
		return zero, map[string]any{
			"error": fmt.Sprintf("%s parameter is required", name),
		}
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	switch any(zero).(type) {
	case int:
		if floatVal, ok := value.(float64); ok {
			return any(int(floatVal)).(T), nil
		}
	case int32:
		if floatVal, ok := value.(float64); ok {
			return any(int32(floatVal)).(T), nil
		}
	case int64:
		if floatVal, ok := value.(float64); ok {
			return any(int64(floatVal)).(T), nil
		}
	}

	return zero, map[string]any{
		"error": fmt.Sprintf("%s parameter must be of type %T, got %T", name, zero, value),
	}
}

// OptionalParam extracts an optional parameter with a default value
func OptionalParam[T any](cp *Params, name string, defaultValue T) (T, map[string]any) {
	value, exists := cp.inputMap[name]
	if !exists {
		return defaultValue, nil
	}

	// Try direct type assertion
	if v, ok := value.(T); ok {
		return v, nil
	}

	// Handle common JSON numeric conversions
	var zero T
	switch any(zero).(type) {
	case int:
		if floatVal, ok := value.(float64); ok {
			return any(int(floatVal)).(T), nil
		}
	case int32:
		if floatVal, ok := value.(float64); ok {
			return any(int32(floatVal)).(T), nil
		}
	case int64:
		if floatVal, ok := value.(float64); ok {
			return any(int64(floatVal)).(T), nil
		}
	}

	return zero, map[string]any{
		"error": fmt.Sprintf("%s parameter must be of type %T, got %T", name, zero, value),
	}
}

// RawInputs returns a copy of the internal parameter map
func (cp *Params) RawInputs() map[string]any {
	// Create a shallow copy of the map
	return maps.Clone(cp.inputMap)
}
