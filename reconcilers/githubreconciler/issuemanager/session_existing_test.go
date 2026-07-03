/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package issuemanager

import (
	"math/rand/v2"
	"testing"
)

type existingTestData struct{ id string }

func (d existingTestData) Equal(o existingTestData) bool { return d.id == o.id }

func TestIssueSessionExisting(t *testing.T) {
	a := existingTestData{id: "a"}
	b := existingTestData{id: "b"}
	s := &IssueSession[existingTestData]{
		existingIssues: []existingIssue[existingTestData]{
			{data: &a},
			{data: &b},
			{data: nil}, // an issue whose embedded data failed to decode is skipped
		},
	}

	got := s.Existing()
	want := []existingTestData{a, b}
	if len(got) != len(want) {
		t.Fatalf("Existing length: got = %d, want = %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Existing[%d]: got = %+v, want = %+v", i, got[i], want[i])
		}
	}
}

func TestIssueSessionMaxDesired(t *testing.T) {
	want := rand.IntN(100) + 1
	s := &IssueSession[existingTestData]{maxDesiredIssues: want}
	if got := s.MaxDesired(); got != want {
		t.Errorf("MaxDesired: got = %d, want = %d", got, want)
	}
}
