/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import "testing"

func TestTrustedAuthorAssociation(t *testing.T) {
	tests := []struct {
		association string
		want        bool
	}{
		{"OWNER", true},
		{"MEMBER", true},
		{"COLLABORATOR", true},
		{"CONTRIBUTOR", false},
		{"FIRST_TIME_CONTRIBUTOR", false},
		{"NONE", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.association, func(t *testing.T) {
			if got := TrustedAuthorAssociation(tc.association); got != tc.want {
				t.Errorf("TrustedAuthorAssociation(%q): got = %v, want = %v", tc.association, got, tc.want)
			}
		})
	}
}
