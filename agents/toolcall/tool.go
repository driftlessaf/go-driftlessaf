/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
)

// ToolCall is a provider-independent representation of a tool call.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// ToolAnnotations describes behavioral hints about a tool. These map directly
// to the MCP protocol's tool annotation fields and help clients display
// appropriate warnings and take appropriate precautions before invoking a tool.
//
// ReadOnly and Idempotent are plain bool because the MCP spec defines their
// absent/nil default as false. Destructive and OpenWorld are *bool because the
// MCP spec defines their absent/nil default as true — a nil pointer means
// "unset, assume true", while a pointer to false means "explicitly not
// destructive/open-world". Proxies must preserve nil to avoid silently
// downgrading tools that are destructive or open-world by default.
type ToolAnnotations struct {
	// ReadOnly indicates the tool does not modify any state. Defaults to false.
	ReadOnly bool
	// Destructive indicates the tool may cause irreversible changes (e.g. delete).
	// Nil means unset; the MCP default is true (assume destructive unless stated).
	Destructive *bool
	// Idempotent indicates repeated calls with the same arguments have the same
	// effect as a single call. Defaults to false.
	Idempotent bool
	// OpenWorld indicates the tool may interact with external systems beyond
	// the immediate context (e.g. web searches, external APIs).
	// Nil means unset; the MCP default is true (assume open-world unless stated).
	OpenWorld *bool
}

// Definition describes a tool's schema (name, description, parameters).
type Definition struct {
	Name        string
	Description string
	Parameters  []Parameter

	// Annotations describes behavioral hints about this tool. When non-nil,
	// these are forwarded to MCP clients that support tool annotations.
	Annotations *ToolAnnotations

	// OutputSchema is an optional JSON Schema describing the tool's response.
	// Used by MCP's Tool.OutputSchema field.
	OutputSchema *Schema

	// InputSchemaDefs holds the "$defs" map from the top-level input schema.
	// Real-world MCP servers (e.g. Google Cloud Run, GKE) use "$defs" and
	// "$ref" to express shared sub-schemas; this field preserves them so they
	// survive a round-trip through extractParameters → mcpTool.
	InputSchemaDefs map[string]*Schema

	// InputSchemaDescription is the "description" field on the top-level input
	// schema object. It is distinct from Description (the tool's own description)
	// and typically contains the proto message description.
	InputSchemaDescription string

	// InputSchemaExtensions holds any top-level input schema keywords that are
	// not otherwise modelled (e.g. "$schema" meta-schema declaration used by
	// Linear and other non-Google MCP servers).
	InputSchemaExtensions map[string]any
}

// Parameter describes a single tool parameter.
//
// The four scalar types ("string", "integer", "boolean", "number") are fully
// backward-compatible: existing tool definitions compile and run unchanged.
// The additional fields (Items, Properties, Enum, etc.) enable complex JSON
// Schema types such as arrays, objects, enums, and schema composition.
type Parameter struct {
	Name        string
	Type        string // "string"|"integer"|"boolean"|"number"|"array"|"object"|"null"
	Description string
	Required    bool

	// Complex type fields — zero values preserve existing scalar behavior.

	// Items is the element schema for array-type parameters (Type=="array").
	Items *Schema
	// Properties defines named sub-schemas for object-type parameters (Type=="object").
	Properties map[string]*Schema
	// PropertyRequired lists property names that are required within an
	// object-type parameter. Distinct from Required, which controls whether
	// the parameter itself must be present in the tool call.
	PropertyRequired []string
	// AdditionalProperties constrains extra properties on an object parameter.
	// Use FalseSchema() to disallow all additional properties.
	AdditionalProperties *Schema
	// Enum restricts the parameter to a fixed set of allowed values.
	Enum []any

	// Numeric constraints.
	Minimum          *float64
	Maximum          *float64
	ExclusiveMinimum *float64
	ExclusiveMaximum *float64
	MultipleOf       *float64

	// String constraints.
	MinLength *int
	MaxLength *int
	Pattern   string

	// Schema metadata.
	Title      string
	Default    any
	Deprecated bool
	Format     string

	// Schema composition.
	OneOf []*Schema
	AnyOf []*Schema
	AllOf []*Schema
	Not   *Schema

	// Ref is a JSON Schema "$ref" value (e.g. "#/$defs/MyType"). When set,
	// other fields may still be present alongside it (JSON Schema allows
	// sibling keywords next to $ref in draft 2019-09+).
	Ref string

	// ReadOnly marks the parameter as read-only (JSON Schema "readOnly").
	ReadOnly bool
	// WriteOnly marks the parameter as write-only (JSON Schema "writeOnly").
	WriteOnly bool

	// Extensions holds vendor-specific extension keywords (e.g. "x-google-identifier").
	// Keys should begin with "x-" per JSON Schema convention.
	Extensions map[string]any
}

// Schema represents a JSON Schema definition used for nested type descriptions
// within a Parameter — for example, the element schema of an array (Items),
// property schemas of an object (Properties), or composition schemas
// (OneOf, AnyOf, AllOf).
type Schema struct {
	Type        string
	Description string
	Title       string
	Format      string

	// Object fields.
	Properties           map[string]*Schema
	Required             []string // required property names within this object schema
	AdditionalProperties *Schema  // use FalseSchema() to disallow additional properties
	// Defs holds "$defs" named sub-schemas. Typically used at the top level of
	// an input schema to define reusable types referenced via $ref.
	Defs map[string]*Schema

	// Array fields.
	Items *Schema

	// Enum restricts values to a fixed set.
	Enum []any

	// Numeric constraints.
	Minimum          *float64
	Maximum          *float64
	ExclusiveMinimum *float64
	ExclusiveMaximum *float64
	MultipleOf       *float64

	// String constraints.
	MinLength *int
	MaxLength *int
	Pattern   string

	// Schema metadata.
	Default    any
	Deprecated bool
	ReadOnly   bool
	WriteOnly  bool

	// Ref is a JSON Schema "$ref" value (e.g. "#/$defs/MyType").
	Ref string

	// Extensions holds vendor-specific extension keywords (e.g. "x-google-identifier").
	Extensions map[string]any

	// Schema composition.
	OneOf []*Schema
	AnyOf []*Schema
	AllOf []*Schema
	Not   *Schema

	// False represents the JSON Schema boolean false schema (matches nothing).
	// Use FalseSchema() rather than setting this field directly. It is used
	// primarily for AdditionalProperties to disallow any extra properties.
	False bool
}

// FalseSchema returns a sentinel Schema that serializes as the JSON Schema
// boolean false (matches nothing). Use for AdditionalProperties to disallow
// any additional properties in an object schema.
func FalseSchema() *Schema {
	return &Schema{False: true}
}

// Ptr returns a pointer to the given value.
// Commonly used to set Destructive and OpenWorld fields on ToolAnnotations,
// which use *bool to distinguish "explicitly false" from "unset (default true)".
func Ptr[T any](v T) *T {
	return &v
}

// SchemaToMap converts a Schema to the map[string]any JSON Schema
// representation. Returns nil for a nil schema. A false schema (FalseSchema)
// returns nil; callers that need to emit the JSON boolean false should check
// s.False before calling (see schemaToAny).
func SchemaToMap(s *Schema) map[string]any {
	if s == nil || s.False {
		return nil
	}
	return schemaToMap(*s)
}

// ParameterToMap converts a Parameter to a JSON Schema property map suitable
// for use in provider-specific tool definitions (Claude, Gemini, OpenAI, MCP).
func ParameterToMap(p Parameter) map[string]any {
	return schemaToMap(p.asSchema())
}

// asSchema projects a Parameter's type-related fields into a Schema so that
// the shared schemaToMap function can be reused for both types.
func (p Parameter) asSchema() Schema {
	return Schema{
		Type:                 p.Type,
		Description:          p.Description,
		Title:                p.Title,
		Format:               p.Format,
		Properties:           p.Properties,
		Required:             p.PropertyRequired,
		AdditionalProperties: p.AdditionalProperties,
		Items:                p.Items,
		Enum:                 p.Enum,
		Minimum:              p.Minimum,
		Maximum:              p.Maximum,
		ExclusiveMinimum:     p.ExclusiveMinimum,
		ExclusiveMaximum:     p.ExclusiveMaximum,
		MultipleOf:           p.MultipleOf,
		MinLength:            p.MinLength,
		MaxLength:            p.MaxLength,
		Pattern:              p.Pattern,
		Default:              p.Default,
		Deprecated:           p.Deprecated,
		ReadOnly:             p.ReadOnly,
		WriteOnly:            p.WriteOnly,
		Ref:                  p.Ref,
		Extensions:           p.Extensions,
		OneOf:                p.OneOf,
		AnyOf:                p.AnyOf,
		AllOf:                p.AllOf,
		Not:                  p.Not,
	}
}

// schemaToAny converts a Schema to its JSON Schema any representation.
// A false schema becomes the boolean false; all others become map[string]any.
func schemaToAny(s *Schema) any {
	if s == nil {
		return nil
	}
	if s.False {
		return false
	}
	return schemaToMap(*s)
}

// schemaToMap converts a non-nil, non-false Schema to map[string]any.
func schemaToMap(s Schema) map[string]any {
	m := make(map[string]any)
	if s.Ref != "" {
		m["$ref"] = s.Ref
	}
	if s.Type != "" {
		m["type"] = s.Type
	}
	if s.Title != "" {
		m["title"] = s.Title
	}
	if s.Description != "" {
		m["description"] = s.Description
	}
	if s.Format != "" {
		m["format"] = s.Format
	}
	if len(s.Defs) > 0 {
		defs := make(map[string]any, len(s.Defs))
		for name, def := range s.Defs {
			defs[name] = schemaToAny(def)
		}
		m["$defs"] = defs
	}
	if len(s.Properties) > 0 {
		props := make(map[string]any, len(s.Properties))
		for name, prop := range s.Properties {
			props[name] = schemaToAny(prop)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.AdditionalProperties != nil {
		m["additionalProperties"] = schemaToAny(s.AdditionalProperties)
	}
	if s.Items != nil {
		m["items"] = schemaToAny(s.Items)
	}
	if len(s.Enum) > 0 {
		m["enum"] = s.Enum
	}
	if s.Minimum != nil {
		m["minimum"] = *s.Minimum
	}
	if s.Maximum != nil {
		m["maximum"] = *s.Maximum
	}
	if s.ExclusiveMinimum != nil {
		m["exclusiveMinimum"] = *s.ExclusiveMinimum
	}
	if s.ExclusiveMaximum != nil {
		m["exclusiveMaximum"] = *s.ExclusiveMaximum
	}
	if s.MultipleOf != nil {
		m["multipleOf"] = *s.MultipleOf
	}
	if s.MinLength != nil {
		m["minLength"] = *s.MinLength
	}
	if s.MaxLength != nil {
		m["maxLength"] = *s.MaxLength
	}
	if s.Pattern != "" {
		m["pattern"] = s.Pattern
	}
	if len(s.OneOf) > 0 {
		oneOf := make([]any, len(s.OneOf))
		for i, sub := range s.OneOf {
			oneOf[i] = schemaToAny(sub)
		}
		m["oneOf"] = oneOf
	}
	if len(s.AnyOf) > 0 {
		anyOf := make([]any, len(s.AnyOf))
		for i, sub := range s.AnyOf {
			anyOf[i] = schemaToAny(sub)
		}
		m["anyOf"] = anyOf
	}
	if len(s.AllOf) > 0 {
		allOf := make([]any, len(s.AllOf))
		for i, sub := range s.AllOf {
			allOf[i] = schemaToAny(sub)
		}
		m["allOf"] = allOf
	}
	if s.Not != nil {
		m["not"] = schemaToAny(s.Not)
	}
	if s.Default != nil {
		m["default"] = s.Default
	}
	if s.Deprecated {
		m["deprecated"] = true
	}
	if s.ReadOnly {
		m["readOnly"] = true
	}
	if s.WriteOnly {
		m["writeOnly"] = true
	}
	maps.Copy(m, s.Extensions)
	return m
}

// Tool defines a tool once with a single handler that works with any provider.
type Tool[Resp any] struct {
	Def     Definition
	Handler func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], result *Resp) map[string]any
}

// Param extracts a required parameter from the tool call args.
// On error, records a bad tool call on the trace and returns an error response.
func Param[T any](call ToolCall, trace interface {
	BadToolCall(string, string, map[string]any, error)
}, name string) (T, map[string]any) {
	v, err := params.Extract[T](call.Args, name)
	if err != nil {
		trace.BadToolCall(call.ID, call.Name, call.Args, fmt.Errorf("missing %s parameter", name))
		return v, params.Error("%s", err)
	}
	return v, nil
}

// OptionalParam extracts an optional parameter from the tool call args.
func OptionalParam[T any](call ToolCall, name string, defaultValue T) (T, map[string]any) {
	v, err := params.ExtractOptional[T](call.Args, name, defaultValue)
	if err != nil {
		return v, params.Error("%s", err)
	}
	return v, nil
}
