/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"strings"
	"testing"
)

// dialedResult stands in for a response type whose affordances vary at runtime: the caller keeps one
// Go type and withholds the fields a given deployment has turned off.
type dialedResult struct {
	_ struct{} `submitresult:"name=submit_dialed,payload=verdict,description=Submit the verdict.,payloadDescription=Verdict payload"`

	Summary  string `json:"summary" jsonschema:"description=What you concluded,required"`
	Blocked  bool   `json:"blocked,omitempty" jsonschema:"description=Whether you are stuck"`
	Question string `json:"question,omitempty" jsonschema:"description=What you need answered,required"`
}

func TestOmitPayloadFieldsWithholdsPropertiesAndRequired(t *testing.T) {
	opts := OptionsForResponse[*dialedResult]()
	opts.OmitPayloadFields = []string{"blocked", "question"}
	tool, err := ClaudeTool(opts)
	if err != nil {
		t.Fatalf("ClaudeTool: %v", err)
	}
	raw, err := json.Marshal(tool.Definition.InputSchema)
	if err != nil {
		t.Fatalf("marshaling input schema: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"summary"`) {
		t.Errorf("schema dropped a property that was not omitted: %s", got)
	}
	for _, field := range []string{"blocked", "question"} {
		if strings.Contains(got, `"`+field+`"`) {
			// A required entry left behind is the sharper failure: the model would be told to
			// send a property the schema no longer declares.
			t.Errorf("schema still carries omitted field %q: %s", field, got)
		}
	}
}

// Omitting is a schema-only edit: a payload that arrives with the withheld field still decodes, which
// is why callers that need the field gone must also drop it from the result they return.
func TestOmitPayloadFieldsLeavesDecodingAlone(t *testing.T) {
	parsed, err := parsePayload[*dialedResult](map[string]any{"summary": "s", "blocked": true})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if !parsed.Blocked {
		t.Errorf("withheld field did not decode: got = %+v, want Blocked=true", parsed)
	}
}

func TestOmitPayloadFieldsRejectsAnUnknownName(t *testing.T) {
	opts := OptionsForResponse[*dialedResult]()
	opts.OmitPayloadFields = []string{"blocked", "blocked_paths"}
	for _, tc := range []struct {
		name  string
		build func() error
	}{
		{"claude", func() error { _, err := ClaudeTool(opts); return err }},
		{"google", func() error { _, err := GoogleTool(opts); return err }},
		{"openai", func() error { _, err := OpenAITool(opts); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build()
			if err == nil {
				t.Fatal("building the submit tool with an unmatched omit name: got = nil error, want an error")
			}
			if !strings.Contains(err.Error(), "blocked_paths") {
				t.Errorf("error does not name the unmatched field: got = %v", err)
			}
		})
	}
}
