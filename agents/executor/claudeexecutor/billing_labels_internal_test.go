/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"strings"
	"testing"
)

func TestVertexBillingLabelsRules(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		keep  bool
	}{
		{name: "lowercase key and value", key: "service_name", value: "example-service", keep: true},
		{name: "empty value", key: "env", value: "", keep: true},
		{name: "empty team", key: "team", value: ""},
		{name: "unknown team", key: "team", value: "unknown"},
		{name: "empty product", key: "product", value: ""},
		{name: "unknown product", key: "product", value: "unknown"},
		{name: "unknown service name", key: "service_name", value: "unknown", keep: true},
		{name: "63-character key", key: "k" + strings.Repeat("a", 62), value: "v", keep: true},
		{name: "64-character key", key: "k" + strings.Repeat("a", 63), value: "v"},
		{name: "63-character value", key: "product", value: strings.Repeat("a", 63), keep: true},
		{name: "64-character value", key: "product", value: strings.Repeat("a", 64)},
		{name: "key starts with a digit", key: "1team", value: "v"},
		{name: "key starts with an underscore", key: "_team", value: "v"},
		{name: "uppercase value", key: "team", value: "Example"},
		{name: "dot in value", key: "team", value: "a.b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := vertexBillingLabels(map[string]string{tc.key: tc.value})
			if _, kept := got[tc.key]; kept != tc.keep {
				t.Errorf("vertexBillingLabels kept %q=%q: got = %t, want = %t", tc.key, tc.value, kept, tc.keep)
			}
		})
	}
}
