/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema

import "github.com/invopop/jsonschema"

// Generator wraps jsonschema.Reflector with project defaults.
type Generator struct {
	reflector jsonschema.Reflector
}

// NewGenerator constructs a generator wired with the defaults we need for tool schemas.
func NewGenerator() *Generator {
	return &Generator{
		reflector: jsonschema.Reflector{
			RequiredFromJSONSchemaTags: true,
			ExpandedStruct:             true,
			AllowAdditionalProperties:  true,
			DoNotReference:             true,
		},
	}
}

// Reflect returns the JSON schema for the provided value.
func (g *Generator) Reflect(v any) *jsonschema.Schema {
	return g.reflector.Reflect(v)
}

// Reflect derives the JSON schema for the provided value using a default generator.
func Reflect(v any) *jsonschema.Schema {
	return NewGenerator().Reflect(v)
}

// ReflectType allocates a zero value of T and reflects it to a schema.
func ReflectType[T any]() *jsonschema.Schema {
	var zero T
	return Reflect(&zero)
}
