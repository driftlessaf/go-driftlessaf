/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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

	// OmitReasoning removes the separate reasoning string from the terminal
	// tool schema. Use it when the structured payload already carries the full
	// reasoning and splitting a large result across two arguments adds no value.
	OmitReasoning bool

	// OmitPayloadFields lists JSON property names to withhold from the payload
	// schema advertised to the model, and from that schema's required list.
	// Response itself is untouched: decoding, the result the agent returns, and
	// the type its trace is recorded under all stay the same, so a caller can
	// hide an affordance behind a runtime dial without maintaining a mirror of
	// the response type for the hidden shape.
	//
	// Withholding a field from the schema is not the same as rejecting it: a
	// model that invents the property anyway still decodes into Response.
	// Callers for whom the hidden field is load-bearing must also ignore it on
	// the way out.
	//
	// Every name must match a property of the reflected schema; an unmatched
	// name fails tool construction rather than leaving the field advertised,
	// so a later json-tag rename cannot silently re-expose it.
	OmitPayloadFields []string

	// FlatPayload advertises each payload property as its own tool
	// parameter, beside reasoning, instead of the payload as one object
	// parameter named PayloadFieldName; PayloadFieldName and
	// PayloadDescription go unused. Claude sometimes writes an object
	// parameter's members as nested parameter tags, which the API cuts off
	// at the first closing tag and delivers as a string; a flat tool of
	// scalar properties has no object parameter to write that way. An
	// object or array property is still one, and no string coercion
	// applies to it. A submit is read by exact property name, and one
	// missing or null for a required property is rejected. A payload
	// property named reasoning needs OmitReasoning.
	FlatPayload bool
}

func (o *Options[Response]) setDefaults() {
	if o.ToolName == "" {
		o.ToolName = "submit_result"
	}
	if o.Description == "" {
		o.Description = "Submit the final result and complete the analysis. " +
			"This call is terminal: it immediately ends the agent loop and the payload you " +
			"provide becomes the final answer returned to the user. Call it exactly once, with " +
			"your complete answer. Never call it with placeholder or probe content (e.g. \"test\") " +
			"to experiment with the schema — the loop terminates on whatever you submit, and your " +
			"real answer will be lost. If a previous call failed validation, resend the full result " +
			"with the corrected shape, not a minimal test value."
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

// flatParameters is a flat payload's tool parameters, which buildOutcome
// reads a submit's arguments by.
type flatParameters struct {
	// names is every payload property, in struct order.
	names []string
	// required is the properties the payload requires, never nil.
	required []string
}

// toolParameters renders, with convert, the submit tool's parameters other
// than reasoning, and names the ones it requires: the payload as the one
// parameter PayloadFieldName, or under FlatPayload each payload property,
// which flat then describes. flat is nil for a nested payload.
func toolParameters[Response, S any](o Options[Response], convert func(*jsonschema.Schema) (S, error)) (params map[string]S, required []string, flat *flatParameters, err error) {
	s, err := o.schemaForResponse()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("derive payload schema: %w", err)
	}
	if !o.FlatPayload {
		s.Description = o.PayloadDescription
		payload, err := convert(s)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("convert payload schema: %w", err)
		}
		return map[string]S{o.PayloadFieldName: payload}, []string{o.PayloadFieldName}, nil, nil
	}
	if s.Properties == nil || s.Properties.Len() == 0 {
		return nil, nil, nil, errors.New("derive payload schema: a flat payload has no properties")
	}
	if _, ok := s.Properties.Get("reasoning"); ok && !o.OmitReasoning {
		return nil, nil, nil, errors.New("derive payload schema: a flat payload's reasoning property collides with the reasoning parameter; set OmitReasoning")
	}
	params = make(map[string]S, s.Properties.Len())
	flat = &flatParameters{names: make([]string, 0, s.Properties.Len()), required: make([]string, len(s.Required))}
	copy(flat.required, s.Required)
	for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
		p, err := convert(pair.Value)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("convert payload property %q: %w", pair.Key, err)
		}
		params[pair.Key] = p
		flat.names = append(flat.names, pair.Key)
	}
	return params, flat.required, flat, nil
}

func (o *Options[Response]) schemaForResponse() (*jsonschema.Schema, error) {
	reflected := o.Generator.Reflect(newResponseValue[Response]())
	if len(o.OmitPayloadFields) == 0 {
		return reflected, nil
	}
	if err := omitProperties(reflected, o.OmitPayloadFields); err != nil {
		return nil, err
	}
	return reflected, nil
}

// omitProperties deletes the named properties from a reflected payload schema
// and drops them from its required list. An unmatched name is an error: the
// caller asked for the property to be hidden, and reporting nothing would
// advertise a field the deployment meant to withhold.
func omitProperties(s *jsonschema.Schema, names []string) error {
	if s == nil {
		return errors.New("omitting payload fields: no reflected schema")
	}
	for _, name := range names {
		if s.Properties == nil {
			return fmt.Errorf("omitting payload field %q: the reflected schema has no properties", name)
		}
		if _, ok := s.Properties.Delete(name); !ok {
			return fmt.Errorf("omitting payload field %q: the reflected schema has no such property", name)
		}
	}
	s.Required = slices.DeleteFunc(s.Required, func(field string) bool {
		return slices.Contains(names, field)
	})
	return nil
}

// schemaToAny is schemaToMap as a property of the map[string]any parameters
// a Claude or OpenAI tool definition carries.
func schemaToAny(s *jsonschema.Schema) (any, error) {
	return schemaToMap(s)
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
