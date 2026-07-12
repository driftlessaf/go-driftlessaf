/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
)

// docFromJSON decodes a JSON literal into the shapes encoding/json produces
// for an any target, matching what Validate receives at runtime.
func docFromJSON(t *testing.T, literal string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(literal), &doc); err != nil {
		t.Fatalf("unmarshal document: %v", err)
	}
	return doc
}

// constrained exercises every constraint class the generator emits from
// struct tags.
type constrained struct {
	Verdict    string   `json:"verdict" jsonschema:"required,enum=benign,enum=malicious"`
	Confidence float64  `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	Count      int      `json:"count,omitempty" jsonschema:"minimum=1,maximum=100"`
	Summary    string   `json:"summary,omitempty" jsonschema:"minLength=3,maxLength=10"`
	ID         string   `json:"id,omitempty" jsonschema:"pattern=^CVE-[0-9]+$"`
	Tags       []string `json:"tags,omitempty" jsonschema:"minItems=1,maxItems=3"`
	Nested     []item   `json:"nested,omitempty"`
}

type item struct {
	Severity string `json:"severity" jsonschema:"required,enum=low,enum=high"`
}

func TestValidate(t *testing.T) {
	t.Parallel()

	s := ReflectType[constrained]()

	tests := []struct {
		name      string
		document  string
		wantPaths []string
	}{{
		name:     "conforming document",
		document: `{"verdict":"benign","confidence":0.5,"count":3,"summary":"okay","id":"CVE-123","tags":["a"],"nested":[{"severity":"low"}]}`,
	}, {
		name:     "minimal document",
		document: `{"verdict":"malicious","confidence":1}`,
	}, {
		name:      "enum violation",
		document:  `{"verdict":"unsure","confidence":0.5}`,
		wantPaths: []string{"verdict"},
	}, {
		name:      "below minimum",
		document:  `{"verdict":"benign","confidence":-0.1}`,
		wantPaths: []string{"confidence"},
	}, {
		name:      "above maximum",
		document:  `{"verdict":"benign","confidence":1.5}`,
		wantPaths: []string{"confidence"},
	}, {
		name:      "integer bounds",
		document:  `{"verdict":"benign","confidence":0,"count":101}`,
		wantPaths: []string{"count"},
	}, {
		name:      "non-integral integer",
		document:  `{"verdict":"benign","confidence":0,"count":1.5}`,
		wantPaths: []string{"count"},
	}, {
		name:      "string too short",
		document:  `{"verdict":"benign","confidence":0,"summary":"ab"}`,
		wantPaths: []string{"summary"},
	}, {
		name:      "string too long",
		document:  `{"verdict":"benign","confidence":0,"summary":"abcdefghijk"}`,
		wantPaths: []string{"summary"},
	}, {
		name:      "pattern mismatch",
		document:  `{"verdict":"benign","confidence":0,"id":"GHSA-xyz"}`,
		wantPaths: []string{"id"},
	}, {
		name:      "too few items",
		document:  `{"verdict":"benign","confidence":0,"tags":[]}`,
		wantPaths: []string{"tags"},
	}, {
		name:      "too many items",
		document:  `{"verdict":"benign","confidence":0,"tags":["a","b","c","d"]}`,
		wantPaths: []string{"tags"},
	}, {
		name:      "nested enum violation carries indexed path",
		document:  `{"verdict":"benign","confidence":0,"nested":[{"severity":"low"},{"severity":"medium"}]}`,
		wantPaths: []string{"nested[1].severity"},
	}, {
		name:      "missing required field",
		document:  `{"confidence":0.5}`,
		wantPaths: []string{"verdict"},
	}, {
		name:      "null required field",
		document:  `{"verdict":null,"confidence":0.5}`,
		wantPaths: []string{"verdict"},
	}, {
		name:     "null optional field treated as absent",
		document: `{"verdict":"benign","confidence":0,"summary":null,"tags":null}`,
	}, {
		name:      "type mismatch reported without cascading noise",
		document:  `{"verdict":"benign","confidence":"high"}`,
		wantPaths: []string{"confidence"},
	}, {
		name:      "root type mismatch",
		document:  `[]`,
		wantPaths: []string{""},
	}, {
		name:      "multiple violations all reported",
		document:  `{"verdict":"unsure","confidence":2,"summary":"ab"}`,
		wantPaths: []string{"verdict", "confidence", "summary"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			violations := Validate(s, docFromJSON(t, tt.document))

			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, v.Path)
			}
			if len(got) != len(tt.wantPaths) {
				t.Fatalf("violations: got = %v, want paths %v", violations, tt.wantPaths)
			}
			for i, want := range tt.wantPaths {
				if got[i] != want {
					t.Errorf("violation[%d] path: got = %q, want = %q", i, got[i], want)
				}
			}
		})
	}
}

func TestValidateIgnoreRequired(t *testing.T) {
	t.Parallel()

	s := ReflectType[constrained]()

	violations := Validate(s, docFromJSON(t, `{"confidence":0.5}`), Options{IgnoreRequired: true})
	if len(violations) != 0 {
		t.Errorf("violations with IgnoreRequired: got = %v, want none", violations)
	}

	// Value constraints still apply to fields that are present.
	violations = Validate(s, docFromJSON(t, `{"verdict":"unsure"}`), Options{IgnoreRequired: true})
	if len(violations) != 1 || violations[0].Path != "verdict" {
		t.Errorf("violations with IgnoreRequired: got = %v, want one at verdict", violations)
	}
}

func TestValidateMessages(t *testing.T) {
	t.Parallel()

	s := ReflectType[constrained]()

	violations := Validate(s, docFromJSON(t, `{"verdict":"unsure","confidence":0}`))
	if len(violations) != 1 {
		t.Fatalf("violations: got = %v, want exactly one", violations)
	}
	msg := violations[0].String()
	for _, want := range []string{"verdict", "unsure", "benign", "malicious"} {
		if !strings.Contains(msg, want) {
			t.Errorf("violation message %q missing %q", msg, want)
		}
	}
}

func TestValidateNilSchema(t *testing.T) {
	t.Parallel()

	if violations := Validate(nil, map[string]any{"anything": true}); violations != nil {
		t.Errorf("violations for nil schema: got = %v, want nil", violations)
	}
}

// TestValidateExclusiveBoundsConstUnique covers the constraint classes the
// generator can emit that TestValidate's reflected struct does not: exclusive
// numeric bounds, const, and uniqueItems. The schemas are built directly so
// the cases do not depend on tag support for these keywords.
func TestValidateExclusiveBoundsConstUnique(t *testing.T) {
	t.Parallel()

	uintp := func(v uint64) *uint64 { return &v }

	tests := []struct {
		name      string
		schema    *jsonschema.Schema
		document  string
		wantPaths []string
	}{{
		name:     "exclusive minimum satisfied",
		schema:   &jsonschema.Schema{Type: "number", ExclusiveMinimum: "0"},
		document: `0.1`,
	}, {
		name:      "exclusive minimum boundary rejected",
		schema:    &jsonschema.Schema{Type: "number", ExclusiveMinimum: "0"},
		document:  `0`,
		wantPaths: []string{""},
	}, {
		name:     "exclusive maximum satisfied",
		schema:   &jsonschema.Schema{Type: "number", ExclusiveMaximum: "1"},
		document: `0.9`,
	}, {
		name:      "exclusive maximum boundary rejected",
		schema:    &jsonschema.Schema{Type: "number", ExclusiveMaximum: "1"},
		document:  `1`,
		wantPaths: []string{""},
	}, {
		name:     "const satisfied",
		schema:   &jsonschema.Schema{Const: "fixed"},
		document: `"fixed"`,
	}, {
		name:      "const violated",
		schema:    &jsonschema.Schema{Const: "fixed"},
		document:  `"other"`,
		wantPaths: []string{""},
	}, {
		name:     "unique items satisfied",
		schema:   &jsonschema.Schema{Type: "array", UniqueItems: true},
		document: `["a","b","c"]`,
	}, {
		name:      "duplicate items rejected with indexed path",
		schema:    &jsonschema.Schema{Type: "array", UniqueItems: true},
		document:  `["a","b","a"]`,
		wantPaths: []string{"[2]"},
	}, {
		name:      "duplicate objects rejected by canonical encoding",
		schema:    &jsonschema.Schema{Type: "array", UniqueItems: true},
		document:  `[{"k":1},{"k":2},{"k":1}]`,
		wantPaths: []string{"[2]"},
	}, {
		name:      "min items enforced alongside uniqueness",
		schema:    &jsonschema.Schema{Type: "array", UniqueItems: true, MinItems: uintp(2)},
		document:  `["a","a"]`,
		wantPaths: []string{"[1]"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			violations := Validate(tt.schema, docFromJSON(t, tt.document))

			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, v.Path)
			}
			if len(got) != len(tt.wantPaths) {
				t.Fatalf("violations: got = %v, want paths %v", violations, tt.wantPaths)
			}
			for i, want := range tt.wantPaths {
				if got[i] != want {
					t.Errorf("violation[%d] path: got = %q, want = %q", i, got[i], want)
				}
			}
		})
	}
}

// TestValidateCombinators covers anyOf, oneOf, and allOf, which the walker
// enforces but no reflected struct tag produces directly.
func TestValidateCombinators(t *testing.T) {
	t.Parallel()

	stringSchema := &jsonschema.Schema{Type: "string"}
	numberSchema := &jsonschema.Schema{Type: "number"}
	shortString := &jsonschema.Schema{Type: "string", MaxLength: func(v uint64) *uint64 { return &v }(3)}

	tests := []struct {
		name      string
		schema    *jsonschema.Schema
		document  string
		wantPaths []string
	}{{
		name:     "anyOf matches one branch",
		schema:   &jsonschema.Schema{AnyOf: []*jsonschema.Schema{stringSchema, numberSchema}},
		document: `5`,
	}, {
		name:      "anyOf matches no branch",
		schema:    &jsonschema.Schema{AnyOf: []*jsonschema.Schema{stringSchema, numberSchema}},
		document:  `true`,
		wantPaths: []string{""},
	}, {
		name:     "oneOf matches exactly one branch",
		schema:   &jsonschema.Schema{OneOf: []*jsonschema.Schema{stringSchema, numberSchema}},
		document: `"text"`,
	}, {
		name:      "oneOf matches no branch",
		schema:    &jsonschema.Schema{OneOf: []*jsonschema.Schema{stringSchema, numberSchema}},
		document:  `true`,
		wantPaths: []string{""},
	}, {
		name:      "oneOf matches two branches",
		schema:    &jsonschema.Schema{OneOf: []*jsonschema.Schema{stringSchema, shortString}},
		document:  `"ab"`,
		wantPaths: []string{""},
	}, {
		name:     "allOf satisfies every branch",
		schema:   &jsonschema.Schema{AllOf: []*jsonschema.Schema{stringSchema, shortString}},
		document: `"ab"`,
	}, {
		name:      "allOf reports each failing branch",
		schema:    &jsonschema.Schema{AllOf: []*jsonschema.Schema{stringSchema, shortString}},
		document:  `"abcdef"`,
		wantPaths: []string{""},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			violations := Validate(tt.schema, docFromJSON(t, tt.document))

			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, v.Path)
			}
			if len(got) != len(tt.wantPaths) {
				t.Fatalf("violations: got = %v, want paths %v", violations, tt.wantPaths)
			}
			for i, want := range tt.wantPaths {
				if got[i] != want {
					t.Errorf("violation[%d] path: got = %q, want = %q", i, got[i], want)
				}
			}
		})
	}
}
