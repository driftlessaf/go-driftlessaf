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

func mustTemplateExecutor(t *testing.T) *internaltemplate.Template[testData] {
	t.Helper()
	te, err := internaltemplate.New[testData]("test-bot", "-pr-data", "PR")
	if err != nil {
		t.Fatalf("creating template executor: %v", err)
	}
	return te
}

func mustEmbedBody(t *testing.T, te *internaltemplate.Template[testData], data *testData) string {
	t.Helper()
	body, err := te.Embed("PR body text", data)
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

	embeddedData := &testData{
		PackageName: fmt.Sprintf("pkg-%d", rand.Int64()),
		Version:     fmt.Sprintf("v%d.%d.%d", rand.IntN(10), rand.IntN(10), rand.IntN(10)),
		Commit:      fmt.Sprintf("abc%d", rand.Int64()),
	}
	bodyWithData := mustEmbedBody(t, te, embeddedData)

	differentData := &testData{
		PackageName: fmt.Sprintf("other-%d", rand.Int64()),
		Version:     "v99.99.99",
		Commit:      fmt.Sprintf("xyz%d", rand.Int64()),
	}

	tests := []struct {
		name        string
		session     Session[testData]
		expected    *testData
		wantRefresh bool
		wantRequeue bool
	}{{
		name: "no PR exists",
		session: Session[testData]{
			manager: &CM[testData]{templateExecutor: te},
		},
		expected:    embeddedData,
		wantRefresh: true,
	}, {
		name: "data matches and mergeable",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: ptrTo(true),
		},
		expected:    embeddedData,
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
		expected:    embeddedData,
		wantRefresh: true,
	}, {
		name: "data matches and unknown mergeability",
		session: Session[testData]{
			manager:     &CM[testData]{templateExecutor: te},
			prNumber:    1,
			prBody:      bodyWithData,
			prMergeable: nil,
		},
		expected:    embeddedData,
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
		expected:    embeddedData,
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
		expected:    embeddedData,
		wantRefresh: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.session.needsRefresh(t.Context(), tt.expected)

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
