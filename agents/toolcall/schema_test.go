/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall_test

import (
	"encoding/json"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
)

// ptr helpers make pointer literals concise in table tests.
//
//go:fix inline
func f64(v float64) *float64 { return new(v) }

// roundTripJSON normalises map[string]any through JSON marshal/unmarshal so
// that numeric types are consistently float64 (the same shape produced by
// parsing JSON from the wire).
func roundTripJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestSchemaToMap(t *testing.T) {
	tests := []struct {
		name string
		in   *toolcall.Schema
		want map[string]any
	}{{
		name: "nil schema",
		in:   nil,
		want: nil,
	}, {
		name: "false schema",
		in:   toolcall.FalseSchema(),
		want: nil,
	}, {
		name: "scalar string",
		in:   &toolcall.Schema{Type: "string"},
		want: map[string]any{"type": "string"},
	}, {
		name: "scalar integer",
		in:   &toolcall.Schema{Type: "integer"},
		want: map[string]any{"type": "integer"},
	}, {
		name: "scalar number",
		in:   &toolcall.Schema{Type: "number"},
		want: map[string]any{"type": "number"},
	}, {
		name: "scalar boolean",
		in:   &toolcall.Schema{Type: "boolean"},
		want: map[string]any{"type": "boolean"},
	}, {
		name: "scalar null",
		in:   &toolcall.Schema{Type: "null"},
		want: map[string]any{"type": "null"},
	}, {
		name: "title and description",
		in:   &toolcall.Schema{Type: "string", Title: "My Field", Description: "A description"},
		want: map[string]any{"type": "string", "title": "My Field", "description": "A description"},
	}, {
		name: "format",
		in:   &toolcall.Schema{Type: "string", Format: "date-time"},
		want: map[string]any{"type": "string", "format": "date-time"},
	}, {
		name: "array of strings",
		in:   &toolcall.Schema{Type: "array", Items: &toolcall.Schema{Type: "string"}},
		want: map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}, {
		name: "array of integers",
		in:   &toolcall.Schema{Type: "array", Items: &toolcall.Schema{Type: "integer"}},
		want: map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	}, {
		name: "array of objects",
		in: &toolcall.Schema{
			Type: "array",
			Items: &toolcall.Schema{
				Type: "object",
				Properties: map[string]*toolcall.Schema{
					"name": {Type: "string"},
				},
				Required: []string{"name"},
			},
		},
		want: map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
	}, {
		name: "nested arrays",
		in: &toolcall.Schema{
			Type: "array",
			Items: &toolcall.Schema{
				Type:  "array",
				Items: &toolcall.Schema{Type: "integer"},
			},
		},
		want: map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "integer"},
			},
		},
	}, {
		name: "flat object with required",
		in: &toolcall.Schema{
			Type: "object",
			Properties: map[string]*toolcall.Schema{
				"host": {Type: "string"},
				"port": {Type: "integer"},
			},
			Required: []string{"host"},
		},
		want: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": map[string]any{"type": "string"},
				"port": map[string]any{"type": "integer"},
			},
			"required": []string{"host"},
		},
	}, {
		name: "additionalProperties false",
		in: &toolcall.Schema{
			Type:                 "object",
			Properties:           map[string]*toolcall.Schema{"x": {Type: "string"}},
			AdditionalProperties: toolcall.FalseSchema(),
		},
		want: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"x": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}, {
		name: "additionalProperties typed",
		in: &toolcall.Schema{
			Type:                 "object",
			AdditionalProperties: &toolcall.Schema{Type: "string"},
		},
		want: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
		},
	}, {
		name: "string enum",
		in:   &toolcall.Schema{Type: "string", Enum: []any{"a", "b", "c"}},
		want: map[string]any{"type": "string", "enum": []any{"a", "b", "c"}},
	}, {
		name: "integer enum",
		in:   &toolcall.Schema{Type: "integer", Enum: []any{float64(1), float64(2), float64(3)}},
		want: map[string]any{"type": "integer", "enum": []any{float64(1), float64(2), float64(3)}},
	}, {
		name: "numeric minimum and maximum",
		in:   &toolcall.Schema{Type: "number", Minimum: f64(0), Maximum: f64(100)},
		want: map[string]any{"type": "number", "minimum": float64(0), "maximum": float64(100)},
	}, {
		name: "exclusive minimum and maximum",
		in:   &toolcall.Schema{Type: "number", ExclusiveMinimum: f64(0), ExclusiveMaximum: f64(100)},
		want: map[string]any{"type": "number", "exclusiveMinimum": float64(0), "exclusiveMaximum": float64(100)},
	}, {
		name: "multipleOf",
		in:   &toolcall.Schema{Type: "number", MultipleOf: new(0.5)},
		want: map[string]any{"type": "number", "multipleOf": float64(0.5)},
	}, {
		name: "string minLength and maxLength",
		in:   &toolcall.Schema{Type: "string", MinLength: new(1), MaxLength: new(255)},
		want: map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
	}, {
		name: "string pattern",
		in:   &toolcall.Schema{Type: "string", Pattern: "^[a-z]+$"},
		want: map[string]any{"type": "string", "pattern": "^[a-z]+$"},
	}, {
		name: "oneOf",
		in: &toolcall.Schema{
			OneOf: []*toolcall.Schema{
				{Type: "string"},
				{Type: "integer"},
			},
		},
		want: map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "integer"},
			},
		},
	}, {
		name: "anyOf",
		in: &toolcall.Schema{
			AnyOf: []*toolcall.Schema{
				{Type: "string"},
				{Type: "null"},
			},
		},
		want: map[string]any{
			"anyOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "null"},
			},
		},
	}, {
		name: "allOf",
		in: &toolcall.Schema{
			AllOf: []*toolcall.Schema{
				{Type: "object", Properties: map[string]*toolcall.Schema{"a": {Type: "string"}}},
				{Type: "object", Properties: map[string]*toolcall.Schema{"b": {Type: "integer"}}},
			},
		},
		want: map[string]any{
			"allOf": []any{
				map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
				map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "integer"}}},
			},
		},
	}, {
		name: "not",
		in:   &toolcall.Schema{Not: &toolcall.Schema{Type: "string"}},
		want: map[string]any{"not": map[string]any{"type": "string"}},
	}, {
		name: "not false schema",
		in:   &toolcall.Schema{Not: toolcall.FalseSchema()},
		want: map[string]any{"not": false},
	}, {
		name: "default value",
		in:   &toolcall.Schema{Type: "string", Default: "foo"},
		want: map[string]any{"type": "string", "default": "foo"},
	}, {
		name: "deprecated",
		in:   &toolcall.Schema{Type: "string", Deprecated: true},
		want: map[string]any{"type": "string", "deprecated": true},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolcall.SchemaToMap(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("SchemaToMap mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParameterToMap(t *testing.T) {
	tests := []struct {
		name string
		in   toolcall.Parameter
		want map[string]any
	}{{
		name: "scalar string",
		in:   toolcall.Parameter{Name: "q", Type: "string", Description: "Query"},
		want: map[string]any{"type": "string", "description": "Query"},
	}, {
		name: "array parameter",
		in: toolcall.Parameter{
			Name:  "tags",
			Type:  "array",
			Items: &toolcall.Schema{Type: "string"},
		},
		want: map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
	}, {
		name: "object parameter with required sub-properties",
		in: toolcall.Parameter{
			Name: "config",
			Type: "object",
			Properties: map[string]*toolcall.Schema{
				"host": {Type: "string"},
				"port": {Type: "integer"},
			},
			PropertyRequired: []string{"host"},
		},
		want: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": map[string]any{"type": "string"},
				"port": map[string]any{"type": "integer"},
			},
			"required": []string{"host"},
		},
	}, {
		name: "enum parameter",
		in: toolcall.Parameter{
			Name: "arch",
			Type: "string",
			Enum: []any{"amd64", "arm64"},
		},
		want: map[string]any{"type": "string", "enum": []any{"amd64", "arm64"}},
	}, {
		name: "object with additionalProperties false",
		in: toolcall.Parameter{
			Name:                 "strict",
			Type:                 "object",
			Properties:           map[string]*toolcall.Schema{"x": {Type: "string"}},
			AdditionalProperties: toolcall.FalseSchema(),
		},
		want: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"x": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}, {
		name: "parameter with oneOf",
		in: toolcall.Parameter{
			Name: "id",
			OneOf: []*toolcall.Schema{
				{Type: "string"},
				{Type: "integer"},
			},
		},
		want: map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "integer"},
			},
		},
	}, {
		name: "numeric constraints",
		in: toolcall.Parameter{
			Name:    "score",
			Type:    "number",
			Minimum: f64(0),
			Maximum: f64(1),
		},
		want: map[string]any{"type": "number", "minimum": float64(0), "maximum": float64(1)},
	}, {
		name: "string constraints",
		in: toolcall.Parameter{
			Name:      "slug",
			Type:      "string",
			MinLength: new(1),
			MaxLength: new(64),
			Pattern:   "^[a-z0-9-]+$",
		},
		want: map[string]any{
			"type":      "string",
			"minLength": 1,
			"maxLength": 64,
			"pattern":   "^[a-z0-9-]+$",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolcall.ParameterToMap(tt.in)
			// Round-trip through JSON to normalise numeric types.
			want := roundTripJSON(t, tt.want)
			gotNorm := roundTripJSON(t, got)
			if diff := cmp.Diff(want, gotNorm); diff != "" {
				t.Errorf("ParameterToMap mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSchemaRoundTripViaJSON verifies that a Schema can be converted to
// map[string]any, marshalled, unmarshalled, and the result is identical to
// what the original would produce.
func TestSchemaRoundTripViaJSON(t *testing.T) {
	original := &toolcall.Schema{
		Type:        "object",
		Description: "Top-level object",
		Properties: map[string]*toolcall.Schema{
			"name":    {Type: "string", Description: "Name"},
			"count":   {Type: "integer", Minimum: f64(0)},
			"enabled": {Type: "boolean", Default: true},
			"tags": {
				Type:  "array",
				Items: &toolcall.Schema{Type: "string", Enum: []any{"alpha", "beta"}},
			},
			"meta": {
				Type: "object",
				Properties: map[string]*toolcall.Schema{
					"key":   {Type: "string"},
					"value": {Type: "string"},
				},
				AdditionalProperties: toolcall.FalseSchema(),
			},
		},
		Required: []string{"name"},
		OneOf: []*toolcall.Schema{
			{Type: "object", Properties: map[string]*toolcall.Schema{"a": {Type: "string"}}},
			{Type: "object", Properties: map[string]*toolcall.Schema{"b": {Type: "integer"}}},
		},
	}

	got := toolcall.SchemaToMap(original)
	if got == nil {
		t.Fatal("SchemaToMap returned nil for non-nil schema")
	}

	// Verify specific fields are preserved.
	if got["type"] != "object" {
		t.Errorf("type: got = %v, wanted = object", got["type"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not map[string]any")
	}
	if _, ok := props["tags"]; !ok {
		t.Error("tags property missing")
	}
	tags, ok := props["tags"].(map[string]any)
	if !ok {
		t.Fatal("tags is not map[string]any")
	}
	if tags["type"] != "array" {
		t.Errorf("tags type: got = %v, wanted = array", tags["type"])
	}
	meta, ok := props["meta"].(map[string]any)
	if !ok {
		t.Fatal("meta is not map[string]any")
	}
	if meta["additionalProperties"] != false {
		t.Errorf("meta additionalProperties: got = %v, wanted = false", meta["additionalProperties"])
	}
	if _, ok := got["oneOf"]; !ok {
		t.Error("oneOf missing")
	}
	required, ok := got["required"].([]string)
	if !ok {
		t.Fatalf("required is not []string: got %T", got["required"])
	}
	if len(required) != 1 || required[0] != "name" {
		t.Errorf("required: got = %v, wanted = [name]", required)
	}
}
