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
