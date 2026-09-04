/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package issuemanager

import (
	"math/rand/v2"
	"slices"
	"testing"
	"text/template"

	"github.com/google/go-github/v88/github"
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

type overlapTestData struct{ ids []string }

func (d overlapTestData) Equal(o overlapTestData) bool {
	return slices.ContainsFunc(d.ids, func(id string) bool {
		return slices.Contains(o.ids, id)
	})
}

func TestFindMatchingIssueOneToOne(t *testing.T) {
	for _, tc := range []struct {
		name            string
		opts            []Option[overlapTestData]
		wantSecondMatch bool
	}{
		{
			name:            "disabled by default",
			wantSecondMatch: true,
		},
		{
			name:            "enabled",
			opts:            []Option[overlapTestData]{WithOneToOneMatching[overlapTestData]()},
			wantSecondMatch: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := template.Must(template.New("issue").Parse("issue"))
			manager, err := New("test-manager", tmpl, tmpl, tc.opts...)
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}

			oldGroup := overlapTestData{ids: []string{"A", "B", "C", "D"}}
			session := &IssueSession[overlapTestData]{
				manager: manager,
				existingIssues: []existingIssue[overlapTestData]{
					{issue: &github.Issue{Number: github.Ptr(1)}, data: &oldGroup},
				},
			}

			left := overlapTestData{ids: []string{"A", "B"}}
			first := session.findMatchingIssue(&left, nil)
			if first == nil {
				t.Fatal("first split group did not match the existing issue")
			}
			matched := map[int]struct{}{first.issue.GetNumber(): {}}

			right := overlapTestData{ids: []string{"C", "D"}}
			second := session.findMatchingIssue(&right, matched)
			if got := second != nil; got != tc.wantSecondMatch {
				t.Errorf("second split group match = %t, want %t", got, tc.wantSecondMatch)
			}
		})
	}
}

func TestCloseMessageForDuplicate(t *testing.T) {
	session := &IssueSession[overlapTestData]{}
	existing := overlapTestData{ids: []string{"B"}}
	desired := overlapTestData{ids: []string{"A", "B"}}

	got := session.closeMessageFor(
		&existing,
		[]*overlapTestData{&desired},
		[]string{"https://example.test/issues/1"},
		"No findings remain.",
	)
	want := "Duplicate of https://example.test/issues/1, closing."
	if got != want {
		t.Errorf("closeMessageFor() = %q, want %q", got, want)
	}

	disjoint := overlapTestData{ids: []string{"C"}}
	if got := session.closeMessageFor(&disjoint, []*overlapTestData{&desired}, []string{"https://example.test/issues/1"}, "No findings remain."); got != "No findings remain." {
		t.Errorf("closeMessageFor() fallback = %q, want %q", got, "No findings remain.")
	}
}
