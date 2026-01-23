/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"fmt"
	"reflect"

	"chainguard.dev/driftlessaf/agents/schema"
	"github.com/invopop/jsonschema"
	"google.golang.org/genai"
)

// Options configures the submit_result tool wiring.
type Options[Response any] struct {
	ToolName           string
	Description        string
	SuccessMessage     string
	PayloadFieldName   string
	PayloadDescription string
	Generator          *schema.Generator
}

func (o *Options[Response]) setDefaults() {
	if o.ToolName == "" {
		o.ToolName = "submit_result"
	}
	if o.Description == "" {
		o.Description = "Submit the final result and complete the analysis."
	}
	if o.SuccessMessage == "" {
		o.SuccessMessage = "Result submitted successfully."
	}
	if o.PayloadFieldName == "" {
		o.PayloadFieldName = "result"
	}
	if o.PayloadDescription == "" {
		o.PayloadDescription = "Structured result payload."
	}
	if o.Generator == nil {
		o.Generator = schema.NewGenerator()
	}
}

func (o *Options[Response]) validate() error {
	if o.PayloadFieldName == "" {
		return fmt.Errorf("payload field name is required")
	}
	return nil
}

func (o *Options[Response]) schemaForResponse() *jsonschema.Schema {
	typ := reflect.TypeFor[Response]()
	var value any
	if typ.Kind() == reflect.Pointer {
		value = reflect.New(typ.Elem()).Interface()
	} else {
		value = reflect.New(typ).Interface()
	}
	return o.Generator.Reflect(value)
}

func schemaToMap(s *jsonschema.Schema) (map[string]any, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func schemaToGenai(s *jsonschema.Schema) *genai.Schema {
	if s == nil {
		return nil
	}

	out := &genai.Schema{
		Description: s.Description,
		Title:       s.Title,
		Format:      s.Format,
	}

	if t := mapSchemaType(s.Type); t != "" {
		out.Type = t
	}

	if len(s.Enum) > 0 {
		out.Enum = make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			out.Enum = append(out.Enum, fmt.Sprint(v))
		}
	}

	if len(s.Required) > 0 {
		out.Required = append(out.Required, s.Required...)
	}

	if len(s.Examples) > 0 {
		out.Example = s.Examples[0]
	}

	if s.Default != nil {
		out.Default = s.Default
	}

	if s.Pattern != "" {
		out.Pattern = s.Pattern
	}

	if s.MaxLength != nil {
		v := int64(*s.MaxLength)
		out.MaxLength = &v
	}
	if s.MinLength != nil {
		v := int64(*s.MinLength)
		out.MinLength = &v
	}
	if s.MaxItems != nil {
		v := int64(*s.MaxItems)
		out.MaxItems = &v
	}
	if s.MinItems != nil {
		v := int64(*s.MinItems)
		out.MinItems = &v
	}
	if s.MaxProperties != nil {
		v := int64(*s.MaxProperties)
		out.MaxProperties = &v
	}
	if s.MinProperties != nil {
		v := int64(*s.MinProperties)
		out.MinProperties = &v
	}
	if !isZeroNumber(s.Maximum) {
		if v, err := s.Maximum.Float64(); err == nil {
			out.Maximum = &v
		}
	}
	if !isZeroNumber(s.Minimum) {
		if v, err := s.Minimum.Float64(); err == nil {
			out.Minimum = &v
		}
	}

	if s.Properties != nil {
		out.Properties = make(map[string]*genai.Schema, s.Properties.Len())
		ordering := make([]string, 0, s.Properties.Len())
		for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
			out.Properties[pair.Key] = schemaToGenai(pair.Value)
			ordering = append(ordering, pair.Key)
		}
		if len(ordering) > 0 {
			out.PropertyOrdering = ordering
		}
	}

	if s.Items != nil {
		out.Items = schemaToGenai(s.Items)
	}

	if len(s.AnyOf) > 0 {
		out.AnyOf = make([]*genai.Schema, 0, len(s.AnyOf))
		for _, child := range s.AnyOf {
			out.AnyOf = append(out.AnyOf, schemaToGenai(child))
		}
	}

	return out
}

func mapSchemaType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	case "null":
		return genai.TypeNULL
	default:
		return ""
	}
}

func isZeroNumber(n json.Number) bool {
	return len(n) == 0
}
