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
					t.Errorf("requeue: got = %v, wanted RequeueAfter error", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tt.wantRefresh {
				t.Errorf("needsRefresh: got = %v, wanted = %v", got, tt.wantRefresh)
			}
		})
	}
}
