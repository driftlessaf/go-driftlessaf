/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
	"chainguard.dev/driftlessaf/workqueue"
)

func mustTemplateExecutor(t *testing.T) *internaltemplate.Template[embeddedData[testData]] {
	t.Helper()
	te, err := internaltemplate.New[embeddedData[testData]]("test-bot", "-pr-data", "PR")
	if err != nil {
		t.Fatalf("creating template executor: %v", err)
	}
	return te
}

func mustEmbedBody(t *testing.T, te *internaltemplate.Template[embeddedData[testData]], data *testData) string {
	t.Helper()
	body, err := te.Embed("PR body text", &embeddedData[testData]{Data: *data})
	if err != nil {
		t.Fatalf("embedding data: %v", err)
	}
	return body
}

func TestSessionGetters(t *testing.T) {
	tests := []struct {
		name          string
		session       Session[testData]
		wantPRNumber  int
		wantAssignees []string
		wantLabels    []string
	}{{
		name:          "no PR",
		session:       Session[testData]{},
		wantPRNumber:  0,
		wantAssignees: nil,
		wantLabels:    nil,
	}, {
		name: "PR with assignees and labels",
		session: Session[testData]{
			prNumber:    42,
			prAssignees: []string{"alice", "bob"},
			prLabels:    []string{"skip:cve-remediation", "automated pr"},
		},
		wantPRNumber:  42,
		wantAssignees: []string{"alice", "bob"},
		wantLabels:    []string{"skip:cve-remediation", "automated pr"},
	}, {
		name: "PR with no assignees and no labels",
		session: Session[testData]{
			prNumber:    7,
			prAssignees: []string{},
			prLabels:    []string{},
		},
		wantPRNumber:  7,
		wantAssignees: []string{},
		wantLabels:    []string{},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.session.PRNumber(); got != tt.wantPRNumber {
				t.Errorf("PRNumber(): got = %d, want = %d", got, tt.wantPRNumber)
			}
			if got := tt.session.Assignees(); !slicesEqual(got, tt.wantAssignees) {
				t.Errorf("Assignees(): got = %v, want = %v", got, tt.wantAssignees)
			}
			if got := tt.session.Labels(); !slicesEqual(got, tt.wantLabels) {
				t.Errorf("Labels(): got = %v, want = %v", got, tt.wantLabels)
			}
		})
	}
}

// slicesEqual returns true if two string slices have the same elements in the same order,
// treating nil and empty slices as unequal.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHasUnresolvedReviews(t *testing.T) {
	tests := []struct {
		name     string
		findings []callbacks.Finding
		want     bool
	}{{
		name:     "no findings",
		findings: nil,
		want:     false,
	}, {
		name:     "only CI check findings",
		findings: []callbacks.Finding{{Kind: callbacks.FindingKindCICheck, Identifier: "1"}},
		want:     false,
	}, {
		name: "review finding among CI findings",
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindCICheck, Identifier: "1"},
			{Kind: callbacks.FindingKindReview, Identifier: "thread-abc"},
		},
		want: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Session[testData]{findings: tt.findings}
			if got := s.HasUnresolvedReviews(); got != tt.want {
				t.Errorf("HasUnresolvedReviews(): got = %v, want = %v", got, tt.want)
			}
		})
	}
}

func TestHitMaxCommitsDynamicBudget(t *testing.T) {
	tests := []struct {
		name        string
		dynamic     bool
		commitCount int
		baseline    int
		want        bool
	}{{
		name:        "static under limit",
		commitCount: 4,
		want:        false,
	}, {
		name:        "static at limit ignores baseline",
		commitCount: 5,
		baseline:    3,
		want:        true,
	}, {
		name:        "dynamic counts commits since baseline",
		dynamic:     true,
		commitCount: 7,
		baseline:    3,
		want:        false,
	}, {
		name:        "dynamic at limit",
		dynamic:     true,
		commitCount: 8,
		baseline:    3,
		want:        true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Session[testData]{
				manager:     &CM[testData]{maxCommits: 5, dynamicCommitBudget: tt.dynamic},
				prNumber:    1,
				prMergeable: ptrTo(true),
				commitCount: tt.commitCount,
				meta:        metadata{CommitBudgetBaseline: tt.baseline},
			}
			if got := s.State().HitMaxCommits(); got != tt.want {
				t.Errorf("HitMaxCommits(): got = %v, want = %v", got, tt.want)
			}
		})
	}
}

func TestResetCommitBudget(t *testing.T) {
	tests := []struct {
		name     string
		dynamic  bool
		prNumber int
		want     int
	}{{
		name:     "resets baseline to commit count",
		dynamic:  true,
		prNumber: 7,
		want:     12,
	}, {
		name:     "no-op when dynamic budget disabled",
		prNumber: 7,
		want:     4,
	}, {
		name:    "no-op when no PR exists",
		dynamic: true,
		want:    4,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session[testData]{
				manager:     &CM[testData]{dynamicCommitBudget: tt.dynamic},
				prNumber:    tt.prNumber,
				commitCount: 12,
				meta:        metadata{CommitBudgetBaseline: 4},
			}
			s.ResetCommitBudget(t.Context())
			if s.meta.CommitBudgetBaseline != tt.want {
				t.Errorf("baseline: got = %d, want = %d", s.meta.CommitBudgetBaseline, tt.want)
			}
		})
	}
}

func TestShouldSkip(t *testing.T) {
	tests := []struct {
		name             string
		session          Session[testData]
		excludeAssignees []string
		wantSkip         bool
	}{{
		name: "no PR",
		session: Session[testData]{
			prNumber: 0,
		},
		wantSkip: false,
	}, {
		name: "PR with skip label",
		session: Session[testData]{
			prNumber: 1,
			prLabels: []string{"skip:test-bot", "automated pr"},
			manager:  &CM[testData]{identity: "test-bot"},
		},
		wantSkip: true,
	}, {
		name: "PR with unmanaged assignee",
		session: Session[testData]{
			prNumber:    2,
			prLabels:    []string{"automated pr"},
			prAssignees: []string{"alice"},
			manager:     &CM[testData]{identity: "test-bot"},
		},
		wantSkip: true,
	}, {
		name: "PR with managed assignee only",
		session: Session[testData]{
			prNumber:    3,
			prLabels:    []string{"automated pr"},
			prAssignees: []string{"alice"},
			manager:     &CM[testData]{identity: "test-bot"},
		},
		excludeAssignees: []string{"alice"},
		wantSkip:         false,
	}, {
		name: "PR with managed and unmanaged assignees",
		session: Session[testData]{
			prNumber:    4,
			prLabels:    []string{"automated pr"},
			prAssignees: []string{"alice", "bob"},
			manager:     &CM[testData]{identity: "test-bot"},
		},
		excludeAssignees: []string{"alice"},
		wantSkip:         true,
	}, {
		name: "PR with no assignees",
		session: Session[testData]{
			prNumber: 5,
			prLabels: []string{},
			manager:  &CM[testData]{identity: "test-bot"},
		},
		wantSkip: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.session.ShouldSkip(tt.excludeAssignees...)
			if got != tt.wantSkip {
				t.Errorf("ShouldSkip(): got = %v, want = %v", got, tt.wantSkip)
			}
		})
	}
}

func TestHasSkipLabel(t *testing.T) {
	tests := []struct {
		name     string
		session  Session[testData]
		wantSkip bool
	}{{
		name: "no PR",
		session: Session[testData]{
			prNumber: 0,
		},
		wantSkip: false,
	}, {
		name: "PR with skip label",
		session: Session[testData]{
			prNumber: 1,
			prLabels: []string{"skip:test-bot", "automated pr"},
			manager:  &CM[testData]{identity: "test-bot"},
		},
		wantSkip: true,
	}, {
		name: "PR with only assignees, no skip label",
		session: Session[testData]{
			prNumber:    2,
			prLabels:    []string{"automated pr"},
			prAssignees: []string{"alice"},
			manager:     &CM[testData]{identity: "test-bot"},
		},
		wantSkip: false,
	}, {
		name: "PR with no labels and no assignees",
		session: Session[testData]{
			prNumber: 3,
			prLabels: []string{},
			manager:  &CM[testData]{identity: "test-bot"},
		},
		wantSkip: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.session.HasSkipLabel()
			if got != tt.wantSkip {
				t.Errorf("HasSkipLabel(): got = %v, want = %v", got, tt.wantSkip)
			}
		})
	}
}

func TestHasLabel(t *testing.T) {
	tests := []struct {
		name      string
		session   Session[testData]
		labelName string
		wantHas   bool
	}{{
		name: "no PR",
		session: Session[testData]{
			prNumber: 0,
		},
		labelName: "test-label",
		wantHas:   false,
	}, {
		name: "PR with label present",
		session: Session[testData]{
			prNumber: 1,
			prLabels: []string{"test-label", "other-label"},
		},
		labelName: "test-label",
		wantHas:   true,
	}, {
		name: "PR with label absent",
		session: Session[testData]{
			prNumber: 2,
			prLabels: []string{"other-label"},
		},
		labelName: "test-label",
		wantHas:   false,
	}, {
		name: "PR with no labels",
		session: Session[testData]{
			prNumber: 3,
			prLabels: []string{},
		},
		labelName: "test-label",
		wantHas:   false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.session.HasLabel(tt.labelName)
			if got != tt.wantHas {
				t.Errorf("HasLabel(%q): got = %v, want = %v", tt.labelName, got, tt.wantHas)
			}
		})
	}
}

func TestNeedsRefresh(t *testing.T) {
	te := mustTemplateExecutor(t)

	sameData := &testData{
		PackageName: fmt.Sprintf("pkg-%d", rand.Int64()),
		Version:     fmt.Sprintf("v%d.%d.%d", rand.IntN(10), rand.IntN(10), rand.IntN(10)),
		Commit:      fmt.Sprintf("abc%d", rand.Int64()),
	}
	bodyWithData := mustEmbedBody(t, te, sameData)

	differentData := &testData{
		PackageName: fmt.Sprintf("other-%d", rand.Int64()),
		Version:     "v99.99.99",
		Commit:      fmt.Sprintf("xyz%d", rand.Int64()),
	}

	tests := []struct {
		name          string
		session       Session[testData]
		expected      *testData
		desiredLabels []string
		wantRefresh   bool
		wantRequeue   bool
	}{{
		name: "no PR exists",
		session: Session[testData]{
			manager: &CM[testData]{templateExecutor: te},
		},
		expected:    sameData,
		wantRefresh: true,
	}, {
		name: "data matches and mergeable",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
		},
		expected:    sameData,
		wantRefresh: false,
	}, {
		name: "data differs and mergeable",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
		},
		expected:    differentData,
		wantRefresh: true,
	}, {
		name: "data matches and needs rebase",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(false),
		},
		expected:    sameData,
		wantRefresh: true,
	}, {
		name: "data matches and unknown mergeability",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: nil,
		},
		expected:    sameData,
		wantRefresh: false,
		wantRequeue: true,
	}, {
		name: "data differs and unknown mergeability - refresh wins over requeue",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: nil,
		},
		expected:    differentData,
		wantRefresh: true,
		wantRequeue: false,
	}, {
		name: "data differs and needs rebase",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(false),
		},
		expected:    differentData,
		wantRefresh: true,
	}, {
		name: "data matches with findings",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te, handlesFindings: true},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
			findings:    []callbacks.Finding{{Kind: callbacks.FindingKindCICheck, Identifier: "1"}},
		},
		expected:    sameData,
		wantRefresh: true,
	}, {
		name: "data matches with pending checks",
		session: Session[testData]{
			manager:       &CM[testData]{templateExecutor: te},
			prNumber:      1,
			prBody:        bodyWithData,
			prMergeable:   ptrTo(true),
			pendingChecks: []string{"ci"},
		},
		expected:    sameData,
		wantRefresh: false,
	}, {
		name: "data matches but managed label no longer desired",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te, managedLabels: []string{"skip:approver-bot"}},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
			prLabels:    []string{"skip:approver-bot"},
		},
		expected:      sameData,
		desiredLabels: nil, // skip:approver-bot is managed, present, but not desired
		wantRefresh:   true,
	}, {
		name: "data matches and managed label still desired",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te, managedLabels: []string{"skip:approver-bot"}},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
			prLabels:    []string{"skip:approver-bot"},
		},
		expected:      sameData,
		desiredLabels: []string{"skip:approver-bot"},
		wantRefresh:   false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.session.needsRefresh(t.Context(), tt.expected, tt.desiredLabels)

			if tt.wantRequeue {
				if _, ok := workqueue.GetRequeueDelay(err); !ok {
					t.Errorf("requeue: got = %v, want RequeueAfter error", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.wantRefresh {
				t.Errorf("needsRefresh: got = %v, want = %v", got, tt.wantRefresh)
			}
		})
	}
}
