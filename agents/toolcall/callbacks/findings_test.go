/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
)

// TestFindingExternalIDNotRendered checks that a finding's ExternalID, check
// provider plumbing, appears in neither the JSON nor the XML an agent is
// shown.
func TestFindingExternalIDNotRendered(t *testing.T) {
	const externalID = "external-id-set-by-the-check-app"
	f := Finding{Kind: FindingKindCICheck, Identifier: "1", Name: "build", ExternalID: externalID}

	asJSON, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	asXML, err := xml.Marshal(f)
	if err != nil {
		t.Fatalf("xml.Marshal: %v", err)
	}
	for format, got := range map[string][]byte{"JSON": asJSON, "XML": asXML} {
		if strings.Contains(string(got), externalID) {
			t.Errorf("%s: got = %s, want = no external ID", format, got)
		}
	}
}
