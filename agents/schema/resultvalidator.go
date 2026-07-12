/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/invopop/jsonschema"
)

// ResultValidator returns the schema-conformance validator that the
// executors install as the base of every submit tool's validator chain: it
// checks a submitted response against the JSON schema reflected from the
// Response type — the same schema (modulo cosmetic descriptions) that the
// submit tool advertises to the model — and returns one finding per
// violation, rejecting the submission back to the model until it conforms.
//
// This is the enforcement half of declaring constraints in jsonschema struct
// tags (enum, minimum/maximum, minLength/maxLength, pattern, minItems, ...):
// the tags shape what the model is asked for, and this validator makes the
// asked-for shape binding. Prefer expressing a constraint as a tag over
// hand-rolling a validator for it; reserve hand-rolled validators for
// cross-field and semantic rules a schema cannot state.
//
// The validator sees the parsed response, not the raw payload, so it
// validates the response's canonical JSON encoding. Required-property checks
// are skipped: after parsing, an omitted field is indistinguishable from its
// zero value (and omitempty fields drop back out), so requiredness cannot be
// checked faithfully here — declare minLength=1 or an enum on fields whose
// zero value is unacceptable.
func ResultValidator[Response any]() callbacks.ResultValidator[Response] {
	s := reflectResponse[Response]()
	return func(_ context.Context, response Response, _ string) ([]callbacks.Finding, error) {
		doc, err := roundTrip(response)
		if err != nil {
			return nil, fmt.Errorf("re-encoding response for schema validation: %w", err)
		}

		violations := Validate(s, doc, Options{IgnoreRequired: true})
		if len(violations) == 0 {
			return nil, nil
		}

		findings := make([]callbacks.Finding, 0, len(violations))
		for _, v := range violations {
			findings = append(findings, callbacks.Finding{
				Kind:       callbacks.FindingKindReview,
				Identifier: schemaFindingID(v.Path),
				Name:       schemaFindingID(v.Path),
				Details:    "the submitted result does not conform to its declared schema — " + v.String(),
			})
		}
		return findings, nil
	}
}

// schemaFindingID names the finding for a violation at the given path.
func schemaFindingID(path string) string {
	if path == "" {
		return "schema"
	}
	return "schema:" + path
}

// reflectResponse derives the JSON schema for the Response type using the
// default generator, dereferencing a pointer type to its element so the
// schema describes the object the model submits.
func reflectResponse[Response any]() *jsonschema.Schema {
	typ := reflect.TypeFor[Response]()
	var value any
	if typ.Kind() == reflect.Pointer {
		value = reflect.New(typ.Elem()).Interface()
	} else {
		value = reflect.New(typ).Interface()
	}
	return NewGenerator().Reflect(value)
}

// roundTrip re-encodes a parsed response as a decoded JSON document — the
// value shapes Validate operates on.
func roundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}
