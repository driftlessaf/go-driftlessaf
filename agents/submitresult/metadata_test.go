/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"testing"
)

type sampleResult struct {
	_ struct{} `submitresult:"name=submit_result,payload=analysis,description=Submit your final analysis result.,payloadDescription=Analysis payload"`

	Summary string `json:"summary" jsonschema:"description=Summary,required"`
}

func TestOptionsForResponseMetadata(t *testing.T) {
	opts := OptionsForResponse[*sampleResult]()
	if opts.PayloadFieldName != "analysis" {
		t.Fatalf("expected payload field 'analysis', got %q", opts.PayloadFieldName)
	}
	if opts.ToolName != "submit_result" {
		t.Fatalf("expected tool name 'submit_result', got %q", opts.ToolName)
	}
}

type escapedCommaResult struct {
	_ struct{} `submitresult:"name=submit_decision,description=Submit the decision. This is the ONLY way to return a result.,payload=decision,payloadDescription=The complete decision: escalate\\, block\\, reasoning\\, and citations.,success=Decision accepted."`

	Reasoning string `json:"reasoning" jsonschema:"description=Why,required"`
}

// TestParseTagEscapedCommas pins the `\,` escape: a comma inside a value is
// content, not a delimiter. Without it the payload description a model reads
// is cut off at its first comma and the trailing keys are lost.
func TestParseTagEscapedCommas(t *testing.T) {
	opts := OptionsForResponse[*escapedCommaResult]()
	if opts.ToolName != "submit_decision" {
		t.Errorf("ToolName = %q, want submit_decision", opts.ToolName)
	}
	if opts.PayloadFieldName != "decision" {
		t.Errorf("PayloadFieldName = %q, want decision", opts.PayloadFieldName)
	}
	if want := "The complete decision: escalate, block, reasoning, and citations."; opts.PayloadDescription != want {
		t.Errorf("PayloadDescription = %q, want %q", opts.PayloadDescription, want)
	}
	if want := "Decision accepted."; opts.SuccessMessage != want {
		t.Errorf("SuccessMessage = %q, want %q (a lost delimiter swallows the trailing keys)", opts.SuccessMessage, want)
	}
	if want := "Submit the decision. This is the ONLY way to return a result."; opts.Description != want {
		t.Errorf("Description = %q, want %q", opts.Description, want)
	}
}

func TestSplitTagParts(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{""}},
		{"a=1,b=2", []string{"a=1", "b=2"}},
		{`a=x\, y,b=2`, []string{"a=x, y", "b=2"}},
		{`a=trailing\,`, []string{"a=trailing,"}},
		{`a=back\slash,b=2`, []string{`a=back\slash`, "b=2"}},
	}
	for _, tc := range cases {
		got := splitTagParts(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitTagParts(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitTagParts(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
