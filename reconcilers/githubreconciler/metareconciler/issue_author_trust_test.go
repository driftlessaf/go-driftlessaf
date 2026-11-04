/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"testing"

	"github.com/google/go-github/v88/github"
)

// TestReconcileIssueTrustedAuthors checks that WithTrustedIssueAuthors runs the
// agent only for an issue from a trusted author, and that without the option
// every author's issue still runs.
func TestReconcileIssueTrustedAuthors(t *testing.T) {
	tests := []struct {
		name        string
		login       string
		typ         string
		association string
		opts        []Option[*testRequest, *publishResult, struct{}]
		wantCalls   int
	}{{
		name: "outside user skipped", login: "stranger", typ: "User", association: "NONE",
		opts: []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]("trusted-app[bot]")},
	}, {
		name: "contributor skipped", login: "drive-by", typ: "User", association: "CONTRIBUTOR",
		opts: []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]()},
	}, {
		name: "member runs", login: "teammate", typ: "User", association: "MEMBER",
		opts:      []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]()},
		wantCalls: 1,
	}, {
		name: "allowlisted app runs", login: "trusted-app[bot]", typ: "Bot", association: "NONE",
		opts:      []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]("trusted-app[bot]")},
		wantCalls: 1,
	}, {
		name: "other app skipped", login: "other-app[bot]", typ: "Bot", association: "NONE",
		opts: []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]("trusted-app[bot]")},
	}, {
		name: "user named like an allowlisted app skipped", login: "trusted-app", typ: "User", association: "NONE",
		opts: []Option[*testRequest, *publishResult, struct{}]{WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]("trusted-app")},
	}, {
		name: "gate off runs any author", login: "stranger", typ: "User", association: "NONE",
		wantCalls: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIssueHarness(t)
			h.issue.User = &github.User{Login: new(tt.login), Type: new(tt.typ)}
			//nolint:staticcheck // SA1019 is about Events API payloads; the Issues REST endpoint the reconciler reads still returns author_association.
			h.issue.AuthorAssociation = new(tt.association)
			h.reconcile(t, func(context.Context) error { return nil }, tt.opts...)
			if h.agentCalls != tt.wantCalls {
				t.Errorf("agent calls: got = %d, want = %d", h.agentCalls, tt.wantCalls)
			}
		})
	}
}

// TestReconcileIssueUntrustedAuthorStillCleansUp checks that an issue from an
// untrusted author still gets the cleanup that removing the managed label
// triggers: the bot's give-up note is deleted, and the agent never runs.
func TestReconcileIssueUntrustedAuthorStillCleansUp(t *testing.T) {
	h := newIssueHarness(t)
	h.issue.User = &github.User{Login: new("stranger"), Type: new("User")}
	//nolint:staticcheck // SA1019 is about Events API payloads; the Issues REST endpoint the reconciler reads still returns author_association.
	h.issue.AuthorAssociation = new("NONE")
	h.addComment(viewerLogin, giveUpMarker+"\nNothing to materialize.")
	h.setLabels()
	h.reconcile(t, func(context.Context) error { return nil },
		withGiveUp(), WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]())
	if h.agentCalls != 0 {
		t.Errorf("agent calls: got = %d, want = 0", h.agentCalls)
	}
	if n := len(h.notes()); n != 0 {
		t.Errorf("bot's notes after the label was removed: got = %d, want = 0", n)
	}
}
