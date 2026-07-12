/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/invopop/jsonschema"
)

// Violation describes one way a decoded JSON value fails to conform to a
// schema. Its message is written for the model that produced the value: it
// names the failing path and states the constraint so the value can be
// corrected and resubmitted.
type Violation struct {
	// Path locates the failing value within the document, e.g.
	// "failures[2].severity". Empty means the document root.
	Path string

	// Message states the violated constraint, e.g.
	// `value "warn" is not one of the allowed values ["error","warning","info"]`.
	Message string
}

// String renders the violation as "<path>: <message>".
func (v Violation) String() string {
	if v.Path == "" {
		return v.Message
	}
	return v.Path + ": " + v.Message
}

// Options adjusts Validate's behavior for callers whose documents cannot
// carry certain constraints faithfully.
type Options struct {
	// IgnoreRequired skips the required-property checks. A document produced
	// by round-tripping a parsed Go value (rather than taken verbatim from
	// the model) no longer distinguishes an omitted field from its zero
	// value — a field with a json omitempty tag drops back out at zero — so
	// required checks against such a document report false positives.
	IgnoreRequired bool
}

// Validate checks a decoded JSON document (map[string]any, []any, string,
// float64, json.Number, bool, or nil — the shapes encoding/json produces)
// against a schema produced by this package's Generator and returns every
// violation found. An empty return means the document conforms.
//
// It enforces the subset of JSON Schema the generator emits from struct
// tags: type, enum, const, numeric bounds (minimum, maximum,
// exclusiveMinimum, exclusiveMaximum), string bounds (minLength, maxLength,
// pattern), array bounds (minItems, maxItems, uniqueItems), object
// requirements (required, properties), items, and the combinators (anyOf,
// oneOf, allOf). Annotation-only keywords (format, description, default,
// examples) and keywords the generator never emits are not enforced.
//
// A null value for a property that is not required is treated as absent and
// skipped: encoding/json decodes it to the field's zero value, which is the
// same outcome as omitting the key. A null for a required property is a
// violation — the requirement exists because a meaningful value is expected.
//
// An invalid pattern in the schema is a schema-authoring bug, not a document
// bug; the pattern constraint is skipped rather than blaming the document.
func Validate(s *jsonschema.Schema, value any, opts ...Options) []Violation {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	var violations []Violation
	(&walker{opts: o}).validateValue(s, value, "", &violations)
	return violations
}

// walker carries the options through the recursive validation.
type walker struct {
	opts Options
}

// validateValue applies the schema to one value, appending violations.
func (w *walker) validateValue(s *jsonschema.Schema, value any, path string, out *[]Violation) {
	if s == nil {
		return
	}

	// Combinators apply regardless of the declared type.
	w.validateCombinators(s, value, path, out)

	if s.Type != "" && !typeMatches(s.Type, value) {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("expected %s, got %s", s.Type, jsonTypeOf(value))})
		// The remaining constraints presume the declared type; reporting
		// them against a mistyped value produces cascading noise.
		return
	}

	if len(s.Enum) > 0 && !enumContains(s.Enum, value) {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %s is not one of the allowed values %s", compactJSON(value), compactJSON(s.Enum))})
	}
	if s.Const != nil && !jsonEqual(s.Const, value) {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %s does not equal the required constant %s", compactJSON(value), compactJSON(s.Const))})
	}

	switch v := value.(type) {
	case string:
		validateString(s, v, path, out)
	case float64:
		validateNumber(s, v, path, out)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			validateNumber(s, f, path, out)
		}
	case []any:
		w.validateArray(s, v, path, out)
	case map[string]any:
		w.validateObject(s, v, path, out)
	}
}

// validateCombinators applies anyOf, oneOf, and allOf.
func (w *walker) validateCombinators(s *jsonschema.Schema, value any, path string, out *[]Violation) {
	if len(s.AnyOf) > 0 && w.countMatches(s.AnyOf, value) == 0 {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %s does not match any of the %d allowed schemas", compactJSON(value), len(s.AnyOf))})
	}
	if len(s.OneOf) > 0 {
		if n := w.countMatches(s.OneOf, value); n != 1 {
			*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %s matches %d of the schemas, exactly one must match", compactJSON(value), n)})
		}
	}
	for _, sub := range s.AllOf {
		w.validateValue(sub, value, path, out)
	}
}

// countMatches reports how many of the schemas the value conforms to.
func (w *walker) countMatches(schemas []*jsonschema.Schema, value any) int {
	matches := 0
	for _, sub := range schemas {
		var vs []Violation
		w.validateValue(sub, value, "", &vs)
		if len(vs) == 0 {
			matches++
		}
	}
	return matches
}

// validateString applies string constraints.
func validateString(s *jsonschema.Schema, v, path string, out *[]Violation) {
	// Length constraints count Unicode code points per the JSON Schema
	// specification, matching what the model sees as characters.
	length := uint64(len([]rune(v)))
	if s.MinLength != nil && length < *s.MinLength {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("length %d is less than the minimum length %d", length, *s.MinLength)})
	}
	if s.MaxLength != nil && length > *s.MaxLength {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("length %d exceeds the maximum length %d", length, *s.MaxLength)})
	}
	if s.Pattern != "" {
		if re, err := regexp.Compile(s.Pattern); err == nil && !re.MatchString(v) {
			*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %s does not match the pattern %q", compactJSON(v), s.Pattern)})
		}
	}
}

// validateNumber applies numeric bounds.
func validateNumber(s *jsonschema.Schema, v float64, path string, out *[]Violation) {
	if s.Type == "integer" && v != math.Trunc(v) {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %v is not an integer", v)})
		return
	}
	if bound, ok := boundValue(s.Minimum); ok && v < bound {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %v is less than the minimum %v", v, bound)})
	}
	if bound, ok := boundValue(s.Maximum); ok && v > bound {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %v exceeds the maximum %v", v, bound)})
	}
	if bound, ok := boundValue(s.ExclusiveMinimum); ok && v <= bound {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %v must be greater than %v", v, bound)})
	}
	if bound, ok := boundValue(s.ExclusiveMaximum); ok && v >= bound {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("value %v must be less than %v", v, bound)})
	}
}

// boundValue parses a json.Number bound; ok is false when the bound is unset
// or unparseable.
func boundValue(n json.Number) (float64, bool) {
	if len(n) == 0 {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}

// validateArray applies array constraints and recurses into items.
func (w *walker) validateArray(s *jsonschema.Schema, v []any, path string, out *[]Violation) {
	length := uint64(len(v))
	if s.MinItems != nil && length < *s.MinItems {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("%d item(s) is fewer than the minimum of %d", length, *s.MinItems)})
	}
	if s.MaxItems != nil && length > *s.MaxItems {
		*out = append(*out, Violation{Path: path, Message: fmt.Sprintf("%d item(s) exceeds the maximum of %d", length, *s.MaxItems)})
	}
	if s.UniqueItems {
		seen := make(map[string]struct{}, len(v))
		for i, item := range v {
			key := compactJSON(item)
			if _, dup := seen[key]; dup {
				*out = append(*out, Violation{Path: itemPath(path, i), Message: "duplicate item in an array whose items must be unique"})
			}
			seen[key] = struct{}{}
		}
	}
	if s.Items != nil {
		for i, item := range v {
			w.validateValue(s.Items, item, itemPath(path, i), out)
		}
	}
}

// validateObject applies required and recurses into declared properties.
func (w *walker) validateObject(s *jsonschema.Schema, v map[string]any, path string, out *[]Violation) {
	if !w.opts.IgnoreRequired {
		for _, name := range s.Required {
			value, present := v[name]
			switch {
			case !present:
				*out = append(*out, Violation{Path: propertyPath(path, name), Message: "required field is missing"})
			case value == nil:
				*out = append(*out, Violation{Path: propertyPath(path, name), Message: "required field is null"})
			}
		}
	}

	if s.Properties == nil {
		return
	}
	for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
		value, present := v[pair.Key]
		if !present {
			continue
		}
		if value == nil {
			// A null optional property decodes to the field's zero value —
			// the same outcome as omitting the key — so it is treated as
			// absent. Null required properties were reported above.
			continue
		}
		w.validateValue(pair.Value, value, propertyPath(path, pair.Key), out)
	}
}

// itemPath renders the path of an array element.
func itemPath(path string, index int) string {
	return fmt.Sprintf("%s[%d]", path, index)
}

// propertyPath renders the path of an object property.
func propertyPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// typeMatches reports whether the decoded value conforms to the declared
// JSON Schema type.
func typeMatches(schemaType string, value any) bool {
	switch schemaType {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		return isJSONNumber(value)
	case "integer":
		// Integrality is checked by validateNumber so the message can be
		// specific; here any JSON number is type-compatible.
		return isJSONNumber(value)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

// isJSONNumber reports whether the value is one of the numeric shapes
// encoding/json produces.
func isJSONNumber(value any) bool {
	switch value.(type) {
	case float64, json.Number:
		return true
	default:
		return false
	}
}

// jsonTypeOf names the JSON type of a decoded value for violation messages.
func jsonTypeOf(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// enumContains reports whether the value equals any of the enum members.
func enumContains(enum []any, value any) bool {
	for _, member := range enum {
		if jsonEqual(member, value) {
			return true
		}
	}
	return false
}

// jsonEqual compares two values by their canonical JSON encoding, which
// normalizes the numeric-representation differences between schema-declared
// members (e.g. int from a struct tag) and decoded document values (float64).
func jsonEqual(a, b any) bool {
	return compactJSON(a) == compactJSON(b)
}

// compactJSON renders a value as compact JSON for comparison and messages.
// Values that cannot marshal (never the case for decoded JSON documents)
// render via fmt as a fallback.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	// Enum message lists read better with a space after commas.
	return strings.ReplaceAll(string(b), `","`, `", "`)
}
