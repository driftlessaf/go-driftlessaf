/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// plainResult does not implement EvidenceDeclarer, like the result of a caller
// that predates evidence.
type plainResult struct{}

func (*plainResult) GetCommitMessage() string { return "Update content" }

func (*plainResult) GetNoChangeExplanation() string { return "Nothing remains to do." }

type plainAgent struct{ execute func(context.Context) error }

func (a *plainAgent) Execute(ctx context.Context, _ *testRequest, _ struct{}) (*plainResult, error) {
	if err := a.execute(ctx); err != nil {
		return nil, err
	}
	return &plainResult{}, nil
}

// reconcilePlain runs one reconcile with a result that declares no evidence.
func (h *issueHarness) reconcilePlain(t *testing.T, agent func(context.Context) error) {
	t.Helper()
	cm, err := changemanager.New[PRData[*testRequest]]("test-bot",
		template.Must(template.New("title").Parse("Update content")),
		template.Must(template.New("body").Parse("Resolve #123.")),
		changemanager.WithFindingsIteration[PRData[*testRequest]]())
	if err != nil {
		t.Fatalf("create change manager: %v", err)
	}
	rec := New("test-bot", cm,
		clonemanager.NewMeta(t.Context(), func(context.Context, string, string) (oauth2.TokenSource, error) {
			return clonemanager.StaticTokenSource(""), nil
		}, "test-bot", nil),
		[]string{requiredLabel},
		&plainAgent{execute: func(ctx context.Context) error {
			h.mu.Lock()
			h.agentCalls++
			h.mu.Unlock()
			return agent(ctx)
		}},
		func(context.Context, *github.Issue, *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(context.Context, *changemanager.Session[PRData[*testRequest]], *clonemanager.Lease) (struct{}, error) {
			return struct{}{}, nil
		},
		WithRequiredLabel[*testRequest, *plainResult, struct{}](requiredLabel),
		WithSteering[*testRequest, *plainResult, struct{}](),
	)
	res := &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 123, Type: githubreconciler.ResourceTypeIssue}
	if err := rec.reconcileIssue(t.Context(), res, h.client(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A result that does not declare evidence never leaves Evidence on the PR,
// and an issue-body edit with no new steer followed by a no-op writes nothing
// to the PR description.
func TestEvidenceNonDeclarerWritesNothing(t *testing.T) {
	h := newIssueHarness(t)
	f := &steeringFake{h: h}
	h.extra = f.serve
	h.setIssueBody(steeringBody())
	h.reconcilePlain(t, writes("one"))
	if len(h.prBodies) != 1 {
		t.Fatalf("PRs created: got = %d, want = 1", len(h.prBodies))
	}
	if e := decodePRData(t, h.prBodies[0]).Evidence; e != nil {
		t.Errorf("evidence at creation: got = %+v, want nil", e)
	}
	h.openPR(t, "MERGEABLE", "[]")
	h.mu.Lock()
	h.prText = h.prBodies[0]
	h.prLabels = []string{requiredLabel}
	h.mu.Unlock()

	h.setIssueBody(steeringBody() + "\nmore detail\n")
	h.reconcilePlain(t, noChange)
	f.wantWrites(t, "non-declarer no-op", 0)
	if e := h.prData(t).Evidence; e != nil {
		t.Errorf("evidence after the no-op: got = %+v, want nil", e)
	}
}
