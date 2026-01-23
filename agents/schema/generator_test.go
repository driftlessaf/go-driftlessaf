/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/judge"
	"chainguard.dev/driftlessaf/agents/schema"
)

func TestReflect(t *testing.T) {
	type nested struct {
		Value string `json:"value" jsonschema:"description=Nested value"`
	}
	type sample struct {
		Name   string  `json:"name" jsonschema:"description=Name,required"`
		Count  int     `json:"count,omitempty"`
		Nested *nested `json:"nested,omitempty"`
	}

	s := schema.Reflect(&sample{})
	if s == nil {
		t.Fatal("expected schema")
	}

	if len(s.Required) != 1 || s.Required[0] != "name" {
		t.Fatalf("unexpected required: %#v", s.Required)
	}

	props := s.Properties
	if props == nil {
		t.Fatal("expected properties")
	}

	name, ok := props.Get("name")
	if !ok {
		t.Fatal("missing name property")
	}
	if name.Description != "Name" {
		t.Fatalf("unexpected description: %q", name.Description)
	}

	nestedSchema, ok := props.Get("nested")
	if !ok {
		t.Fatal("missing nested property")
	}
	nestedProps := nestedSchema.Properties
	if nestedProps == nil {
		t.Fatal("expected nested properties")
	}
	valueSchema, ok := nestedProps.Get("value")
	if !ok {
		t.Fatal("missing nested value property")
	}
	if valueSchema.Description != "Nested value" {
		t.Fatalf("unexpected nested description: %q", valueSchema.Description)
	}
}

// TestRealAgentTypeSchemas validates schema generation works correctly with actual production agent types,
// ensuring all types from simple to complex have proper structure and descriptions.
func TestRealAgentTypeSchemas(t *testing.T) {
	tests := []struct {
		name string
		test func(t *testing.T)
	}{{
		name: "validates_schema_structure",
		test: func(t *testing.T) {
			// This test validates that the schema generator produces correct structure
			// For specific agent type validation, see agents/internal/tooling/submitresulttest/schema_test.go
			// which tests the full integration with real agent response types.

			type simpleStruct struct {
				Name  string `json:"name" jsonschema:"description=Name field"`
				Count int    `json:"count" jsonschema:"description=Count field"`
			}

			s := schema.Reflect(&simpleStruct{})

			// Validate basic structure
			if s.Type != "object" {
				t.Errorf("expected object type, got %s", s.Type)
			}

			// Validate properties exist
			nameProp, ok := s.Properties.Get("name")
			if !ok {
				t.Error("missing name property")
			}
			if nameProp.Description != "Name field" {
				t.Errorf("expected description 'Name field', got %q", nameProp.Description)
			}
		},
	}, {
		name: "handles_nested_structures",
		test: func(t *testing.T) {
			type innerStruct struct {
				Value string `json:"value" jsonschema:"description=Inner value"`
			}
			type outerStruct struct {
				Items []innerStruct `json:"items" jsonschema:"description=List of items"`
			}

			s := schema.Reflect(&outerStruct{})

			itemsProp, ok := s.Properties.Get("items")
			if !ok || itemsProp.Type != "array" {
				t.Error("items should be array")
			}
			if itemsProp.Description != "List of items" {
				t.Errorf("expected description, got %q", itemsProp.Description)
			}

			// Validate nested object in array
			if itemsProp.Items.Type != "object" {
				t.Error("items should contain objects")
			}
			valueProp, ok := itemsProp.Items.Properties.Get("value")
			if !ok {
				t.Error("missing nested value property")
			}
			if valueProp.Description != "Inner value" {
				t.Errorf("expected nested description, got %q", valueProp.Description)
			}
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, tt.test)
	}
}

// TestJudgeTypeSchema validates that the judge result type generates correct schema.
// Tests for other agent types (fixer, loganalyzer, qackage, version) remain in
// agents/internal/schema/generator_test.go since those types are private.
func TestJudgeTypeSchema(t *testing.T) {
	tests := []struct {
		name           string
		responseType   any
		expectedSchema string // JSON representation of expected schema
	}{{
		name:         "judge_judgement",
		responseType: &judge.Judgement{},
		expectedSchema: `{
			"type": "object",
			"properties": {
				"mode": {
					"type": "string"
				},
				"score": {
					"type": "number"
				},
				"reasoning": {
					"type": "string"
				},
				"suggestions": {
					"type": "array",
					"items": {
						"type": "string"
					}
				}
			}
		}`,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			generatedSchema := schema.Reflect(tt.responseType)
			if generatedSchema == nil {
				t.Fatal("expected schema to be generated")
			}

			actualJSON, err := json.MarshalIndent(generatedSchema, "", "  ")
			if err != nil {
				t.Fatalf("failed to marshal actual schema: %v", err)
			}

			var expected map[string]any
			if err := json.Unmarshal([]byte(tt.expectedSchema), &expected); err != nil {
				t.Fatalf("failed to parse expected schema: %v", err)
			}

			var actual map[string]any
			if err := json.Unmarshal(actualJSON, &actual); err != nil {
				t.Fatalf("failed to parse actual schema: %v", err)
			}

			if err := compareSchemas(expected, actual, ""); err != nil {
				t.Errorf("Schema mismatch:\n%s\n\nActual schema:\n%s", err, string(actualJSON))
			}
		})
	}
}

// compareSchemas recursively compares expected (subset) with actual schema
func compareSchemas(expected, actual map[string]any, path string) error {
	for key, expectedVal := range expected {
		currentPath := path + "." + key
		if currentPath == "."+key {
			currentPath = key
		}

		actualVal, ok := actual[key]
		if !ok {
			return fmt.Errorf("missing key %q in actual schema", currentPath)
		}

		switch expV := expectedVal.(type) {
		case map[string]any:
			actV, ok := actualVal.(map[string]any)
			if !ok {
				return fmt.Errorf("at %q: expected object, got %T", currentPath, actualVal)
			}
			if err := compareSchemas(expV, actV, currentPath); err != nil {
				return err
			}

		case []any:
			actV, ok := actualVal.([]any)
			if !ok {
				return fmt.Errorf("at %q: expected array, got %T", currentPath, actualVal)
			}
			if len(expV) > 0 && len(actV) > 0 {
				if expObj, ok := expV[0].(map[string]any); ok {
					if actObj, ok := actV[0].(map[string]any); ok {
						if err := compareSchemas(expObj, actObj, currentPath+"[0]"); err != nil {
							return err
						}
					}
				}
			}

		case string:
			actV, ok := actualVal.(string)
			if !ok {
				return fmt.Errorf("at %q: expected string %q, got %T=%v", currentPath, expV, actualVal, actualVal)
			}
			if !strings.EqualFold(expV, actV) {
				return fmt.Errorf("at %q: expected %q, got %q", currentPath, expV, actV)
			}

		default:
			if expectedVal != actualVal {
				return fmt.Errorf("at %q: expected %v, got %v", currentPath, expectedVal, actualVal)
			}
		}
	}
	return nil
}
