/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool

import (
	"encoding/json"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/toolcall/params"
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
	v, err := params.Extract[T](cp.inputMap, name)
	if err != nil {
		return v, params.Error("%s", err)
	}
	return v, nil
}

// OptionalParam extracts an optional parameter with a default value
func OptionalParam[T any](cp *Params, name string, defaultValue T) (T, map[string]any) {
	v, err := params.ExtractOptional[T](cp.inputMap, name, defaultValue)
	if err != nil {
		return v, params.Error("%s", err)
	}
	return v, nil
}

// RawInputs returns a copy of the internal parameter map
func (cp *Params) RawInputs() map[string]any {
	// Create a shallow copy of the map
	return maps.Clone(cp.inputMap)
}
