/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/nested-schema.golden.json")

// goldenResult covers the schema features a nested payload carries: an enum,
// an optional field, a nested object, and an array of objects.
type goldenResult struct {
	_ struct{} `submitresult:"name=submit_golden,payload=verdict,description=Submit the verdict.,payloadDescription=The verdict.,success=Verdict submitted."`

	Decision string         `json:"decision" jsonschema:"required,enum=approve,enum=reject,description=The decision."`
	Note     string         `json:"note,omitempty" jsonschema:"description=An optional note."`
	Target   goldenTarget   `json:"target" jsonschema:"required"`
	Findings []goldenTarget `json:"findings,omitempty"`
}

type goldenTarget struct {
	Path string `json:"path" jsonschema:"required"`
	Line int    `json:"line,omitempty"`
}

// TestNestedPayloadSchemaGolden: every provider's submit tool for a nested
// payload matches testdata/nested-schema.golden.json, which the builders
// wrote before FlatPayload existed.
func TestNestedPayloadSchemaGolden(t *testing.T) {
	defs := map[string]any{}
	for _, omit := range []bool{false, true} {
		opts := OptionsForResponse[*goldenResult]()
		opts.OmitReasoning = omit
		claude, err := ClaudeTool(opts)
		if err != nil {
			t.Fatalf("ClaudeTool: %v", err)
		}
		google, err := GoogleTool(opts)
		if err != nil {
			t.Fatalf("GoogleTool: %v", err)
		}
		openai, err := OpenAITool(opts)
		if err != nil {
			t.Fatalf("OpenAITool: %v", err)
		}
		responses, err := ResponsesTool(opts)
		if err != nil {
			t.Fatalf("ResponsesTool: %v", err)
		}
		defs[fmt.Sprintf("omitReasoning=%t", omit)] = map[string]any{
			"claude":    claude.Definition,
			"google":    google.Definition,
			"openai":    openai.Definition,
			"responses": responses.Definition,
		}
	}
	got, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		t.Fatalf("encoding the definitions: %v", err)
	}
	const golden = "testdata/nested-schema.golden.json"
	if *updateGolden {
		if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("writing %s: %v", golden, err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s: %v", golden, err)
	}
	if diff := cmp.Diff(string(want), string(got)+"\n"); diff != "" {
		t.Errorf("nested submit tool definitions (-want, +got):\n%s", diff)
	}
}
