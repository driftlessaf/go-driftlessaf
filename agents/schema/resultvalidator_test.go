/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package schema

import (
	"strings"
	"testing"
)

// gradedVerdict is a response type carrying the constraint classes the base
// validator must enforce on parsed values.
type gradedVerdict struct {
	Verdict    string  `json:"verdict" jsonschema:"required,enum=benign,enum=malicious"`
	Confidence float64 `json:"confidence" jsonschema:"minimum=0,maximum=1"`
	Summary    string  `json:"summary,omitempty" jsonschema:"minLength=3"`
	Optional   string  `json:"optional,omitempty" jsonschema:"required"`
}

func TestResultValidatorAcceptsConformingResponse(t *testing.T) {
	t.Parallel()

	validate := ResultValidator[*gradedVerdict]()
	findings, err := validate(t.Context(), &gradedVerdict{
		Verdict:    "benign",
		Confidence: 0.5,
		Summary:    "fine",
	}, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %v, want none", findings)
	}
}

func TestResultValidatorRejectsViolations(t *testing.T) {
	t.Parallel()

	validate := ResultValidator[*gradedVerdict]()
	findings, err := validate(t.Context(), &gradedVerdict{
		Verdict:    "unsure",
		Confidence: 1.5,
		Summary:    "ab",
	}, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}

	wantIdentifiers := []string{"schema:verdict", "schema:confidence", "schema:summary"}
	if len(findings) != len(wantIdentifiers) {
		t.Fatalf("findings: got = %v, want identifiers %v", findings, wantIdentifiers)
	}
	for i, want := range wantIdentifiers {
		if findings[i].Identifier != want {
			t.Errorf("finding[%d] identifier: got = %q, want = %q", i, findings[i].Identifier, want)
		}
		if !strings.Contains(findings[i].Details, "does not conform") {
			t.Errorf("finding[%d] details: got = %q, want mention of nonconformance", i, findings[i].Details)
		}
	}
}

func TestResultValidatorZeroValueEnumStillRejected(t *testing.T) {
	t.Parallel()

	// A model that omits a required enum field parses to the zero value; the
	// enum constraint still catches it even though required checks cannot run
	// against a round-tripped document.
	validate := ResultValidator[*gradedVerdict]()
	findings, err := validate(t.Context(), &gradedVerdict{Confidence: 0.5}, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}
	if len(findings) != 1 || findings[0].Identifier != "schema:verdict" {
		t.Errorf("findings: got = %v, want one at schema:verdict", findings)
	}
}

func TestResultValidatorIgnoresRequiredOnOmitemptyZero(t *testing.T) {
	t.Parallel()

	// Optional is tagged required but carries json omitempty: at the zero
	// value it drops out of the round-tripped document, which must NOT be
	// reported as a missing required field.
	validate := ResultValidator[*gradedVerdict]()
	findings, err := validate(t.Context(), &gradedVerdict{
		Verdict:    "benign",
		Confidence: 0.5,
	}, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %v, want none", findings)
	}
}

func TestResultValidatorNilPointerResponse(t *testing.T) {
	t.Parallel()

	// A null payload parses to a nil pointer without error; the base
	// validator reports the type mismatch instead of letting a nil result
	// commit.
	validate := ResultValidator[*gradedVerdict]()
	findings, err := validate(t.Context(), nil, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}
	if len(findings) != 1 || findings[0].Identifier != "schema" {
		t.Errorf("findings: got = %v, want one root schema finding", findings)
	}
}

func TestResultValidatorValueType(t *testing.T) {
	t.Parallel()

	// Non-pointer response types validate the same way.
	validate := ResultValidator[gradedVerdict]()
	findings, err := validate(t.Context(), gradedVerdict{Verdict: "malicious", Confidence: 2}, "reasoning")
	if err != nil {
		t.Fatalf("validate: got = %v, want = nil", err)
	}
	if len(findings) != 1 || findings[0].Identifier != "schema:confidence" {
		t.Errorf("findings: got = %v, want one at schema:confidence", findings)
	}
}
