/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	internaltemplate "chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/template"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-github/v88/github"
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

func TestAppendReasoning(t *testing.T) {
	entry := func(i int) ReasoningEntry {
		return ReasoningEntry{
			CommitHeadline: fmt.Sprintf("fix: change %d", i),
			Summary:        fmt.Sprintf("- rationale %d", i),
		}
	}

	tests := []struct {
		name     string
		initial  []ReasoningEntry
		headline string
		summary  string
		want     []ReasoningEntry
	}{{
		name:     "appends to empty log",
		headline: "fix: change 1",
		summary:  "- rationale 1",
		want:     []ReasoningEntry{entry(1)},
	}, {
		name:     "appends after existing entries",
		initial:  []ReasoningEntry{entry(1)},
		headline: "fix: change 2",
		summary:  "- rationale 2",
		want:     []ReasoningEntry{entry(1), entry(2)},
	}, {
		name:     "empty summary is a no-op",
		initial:  []ReasoningEntry{entry(1)},
		headline: "fix: change 2",
		summary:  "",
		want:     []ReasoningEntry{entry(1)},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session[testData]{meta: metadata{ReasoningLog: tt.initial}}
			s.AppendReasoning(tt.headline, tt.summary)
			if got := s.ReasoningLog(); !slices.Equal(got, tt.want) {
				t.Errorf("ReasoningLog(): got = %v, want = %v", got, tt.want)
			}
		})
	}
}

// TestAppendReasoningCap verifies the log is bounded: once it holds
// maxReasoningEntries entries, appending drops the oldest so the PR body
// cannot grow without limit.
func TestAppendReasoningCap(t *testing.T) {
	s := &Session[testData]{}
	total := maxReasoningEntries + 3
	for i := range total {
		s.AppendReasoning(fmt.Sprintf("fix: change %d", i), fmt.Sprintf("- rationale %d", i))
	}

	got := s.ReasoningLog()
	if len(got) != maxReasoningEntries {
		t.Fatalf("log length: got = %d, want = %d", len(got), maxReasoningEntries)
	}
	if want := fmt.Sprintf("fix: change %d", total-maxReasoningEntries); got[0].CommitHeadline != want {
		t.Errorf("oldest retained headline: got = %q, want = %q", got[0].CommitHeadline, want)
	}
	if want := fmt.Sprintf("fix: change %d", total-1); got[len(got)-1].CommitHeadline != want {
		t.Errorf("newest headline: got = %q, want = %q", got[len(got)-1].CommitHeadline, want)
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

func TestIssueHasSkipLabel(t *testing.T) {
	issueWith := func(names ...string) *github.Issue {
		labels := make([]*github.Label, 0, len(names))
		for _, n := range names {
			labels = append(labels, &github.Label{Name: github.Ptr(n)})
		}
		return &github.Issue{Labels: labels}
	}
	tests := []struct {
		name  string
		issue *github.Issue
		want  bool
	}{{
		name:  "issue with skip label",
		issue: issueWith("other", "skip:test-bot"),
		want:  true,
	}, {
		name:  "issue without skip label",
		issue: issueWith("test-bot/managed", "automated"),
		want:  false,
	}, {
		name:  "issue with no labels",
		issue: issueWith(),
		want:  false,
	}}

	s := Session[testData]{manager: &CM[testData]{identity: "test-bot"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.IssueHasSkipLabel(tt.issue); got != tt.want {
				t.Errorf("IssueHasSkipLabel(): got = %v, want = %v", got, tt.want)
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

// TestSessionStateLabelAccessors verifies the identity-derived label accessors
// the metareconciler uses to detect newly-applied state labels (the edges it
// emits state-transition events on).
func TestSessionStateLabelAccessors(t *testing.T) {
	cm := &CM[testData]{identity: "test-bot"}

	tests := []struct {
		name               string
		session            Session[testData]
		wantTurnLimit      bool
		wantReadyForReview bool
		wantGaveUp         bool
		wantPRURL          string
	}{{
		name:    "no PR has no labels",
		session: Session[testData]{manager: cm},
	}, {
		name: "PR with all state labels",
		session: Session[testData]{
			manager:  cm,
			prNumber: 42,
			prURL:    "https://github.com/o/r/pull/42",
			prLabels: []string{
				"test-bot/turn-limit",
				"test-bot/ready-for-review",
				"test-bot/too-hard-need-human",
			},
		},
		wantTurnLimit:      true,
		wantReadyForReview: true,
		wantGaveUp:         true,
		wantPRURL:          "https://github.com/o/r/pull/42",
	}, {
		name: "another identity's labels do not match",
		session: Session[testData]{
			manager:  cm,
			prNumber: 7,
			prURL:    "https://github.com/o/r/pull/7",
			prLabels: []string{
				"other-bot/turn-limit",
				"other-bot/ready-for-review",
				"other-bot/too-hard-need-human",
			},
		},
		wantPRURL: "https://github.com/o/r/pull/7",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.session.HasTurnLimitLabel(); got != tt.wantTurnLimit {
				t.Errorf("HasTurnLimitLabel(): got = %v, want = %v", got, tt.wantTurnLimit)
			}
			if got := tt.session.HasReadyForReviewLabel(); got != tt.wantReadyForReview {
				t.Errorf("HasReadyForReviewLabel(): got = %v, want = %v", got, tt.wantReadyForReview)
			}
			if got := tt.session.HasGaveUpLabel(); got != tt.wantGaveUp {
				t.Errorf("HasGaveUpLabel(): got = %v, want = %v", got, tt.wantGaveUp)
			}
			if got := tt.session.PRURL(); got != tt.wantPRURL {
				t.Errorf("PRURL(): got = %q, want = %q", got, tt.wantPRURL)
			}
		})
	}
}
