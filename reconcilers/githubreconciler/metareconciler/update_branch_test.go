/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v88/github"
)

const updateBranchLabel = "test-bot/update-branch"

// fakeUpdateBranch answers the REST requests the update-branch case makes.
type fakeUpdateBranch struct {
	compareStatus string // status of default branch...PR head
	updateCode    int    // response code of the update-branch call
	removeCode    int    // response code of the label removal
	headRepo      string // full name of the PR head repository; defaults to owner/repo

	updates      int
	expectedHead string
	removals     int
}

func (f *fakeUpdateBranch) serve(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
		fmt.Fprint(w, `{"default_branch":"main"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/456":
		headRepo := f.headRepo
		if headRepo == "" {
			headRepo = "owner/repo"
		}
		fmt.Fprintf(w, `{"number":456,"head":{"ref":"branch","sha":"abc123","repo":{"full_name":%q}}}`, headRepo)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/compare/main...abc123":
		fmt.Fprintf(w, `{"status":%q}`, f.compareStatus)
	case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/pulls/456/update-branch":
		f.updates++
		var body struct {
			ExpectedHeadSHA string `json:"expected_head_sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.expectedHead = body.ExpectedHeadSHA
		w.WriteHeader(f.updateCode)
		fmt.Fprint(w, `{"message":"Updating pull request branch.","url":"https://github.com/owner/repo/pull/456"}`)
	case r.Method == http.MethodDelete && r.URL.Path == "/repos/owner/repo/issues/456/labels/"+updateBranchLabel:
		f.removals++
		w.WriteHeader(f.removeCode)
		if f.removeCode != http.StatusOK {
			fmt.Fprint(w, `{"message":"nope"}`)
			return true
		}
		fmt.Fprint(w, `[]`)
	default:
		return false
	}
	return true
}

func TestWithUpdateBranchLabel(t *testing.T) {
	r := &Reconciler[*testRequest, *testResult, testCallbacks]{}
	WithUpdateBranchLabel[*testRequest, *testResult, testCallbacks](updateBranchLabel)(r)
	if r.updateBranchLabel != updateBranchLabel {
		t.Errorf("reconciler.updateBranchLabel = %q, want = %q", r.updateBranchLabel, updateBranchLabel)
	}
}

func TestReconcileIssueUpdateBranch(t *testing.T) {
	withOpt := []Option[*testRequest, *publishResult, struct{}]{
		WithUpdateBranchLabel[*testRequest, *publishResult, struct{}](updateBranchLabel),
	}
	tests := []struct {
		name         string
		fake         fakeUpdateBranch
		prLabels     []string
		opts         []Option[*testRequest, *publishResult, struct{}]
		untrusted    bool
		wantErr      bool
		wantUpdates  int
		wantRemovals int
	}{{
		name:         "label present updates the branch and removes the label",
		fake:         fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantUpdates:  1,
		wantRemovals: 1,
	}, {
		name:         "branch behind the default branch is updated",
		fake:         fakeUpdateBranch{compareStatus: "behind", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantUpdates:  1,
		wantRemovals: 1,
	}, {
		name:         "already up to date only removes the label",
		fake:         fakeUpdateBranch{compareStatus: "ahead", removeCode: http.StatusOK},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantRemovals: 1,
	}, {
		name:         "fork head only removes the label",
		fake:         fakeUpdateBranch{compareStatus: "diverged", headRepo: "someone/repo", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantRemovals: 1,
	}, {
		name:         "422 removes the label",
		fake:         fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusUnprocessableEntity, removeCode: http.StatusOK},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantUpdates:  1,
		wantRemovals: 1,
	}, {
		name:        "other error is retried with the label kept",
		fake:        fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusInternalServerError, removeCode: http.StatusOK},
		prLabels:    []string{updateBranchLabel},
		opts:        withOpt,
		wantErr:     true,
		wantUpdates: 1,
	}, {
		name:         "label removal failure is retried",
		fake:         fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusInternalServerError},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantErr:      true,
		wantUpdates:  1,
		wantRemovals: 1,
	}, {
		name:         "label already gone counts as removed",
		fake:         fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusNotFound},
		prLabels:     []string{updateBranchLabel},
		opts:         withOpt,
		wantUpdates:  1,
		wantRemovals: 1,
	}, {
		name:     "skip label leaves the label alone",
		fake:     fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels: []string{updateBranchLabel, "skip:test-bot"},
		opts:     withOpt,
	}, {
		name:      "untrusted author does not update",
		fake:      fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels:  []string{updateBranchLabel},
		untrusted: true,
		opts: append([]Option[*testRequest, *publishResult, struct{}]{
			WithTrustedIssueAuthors[*testRequest, *publishResult, struct{}]()}, withOpt...),
	}, {
		name:     "option off ignores the label",
		fake:     fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		prLabels: []string{updateBranchLabel},
	}, {
		name: "label absent ignores the option",
		fake: fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusOK},
		opts: withOpt,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIssueHarness(t)
			h.openPR(t, "MERGEABLE", "[]")
			h.prLabels = tt.prLabels
			h.extra = tt.fake.serve
			if tt.untrusted {
				h.issue.User = &github.User{Login: new("stranger"), Type: new("User")}
				//nolint:staticcheck // SA1019 is about Events API payloads; the Issues REST endpoint the reconciler reads still returns author_association.
				h.issue.AuthorAssociation = new("NONE")
			}
			err := h.reconcileErr(t, t.Context(), func(context.Context) error { return nil }, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("reconcile error: got = %v, want error = %v", err, tt.wantErr)
			}
			f := &tt.fake
			if f.updates != tt.wantUpdates {
				t.Errorf("update calls: got = %d, want = %d", f.updates, tt.wantUpdates)
			}
			if f.updates > 0 && f.expectedHead != "abc123" {
				t.Errorf("expected_head_sha: got = %q, want = %q", f.expectedHead, "abc123")
			}
			if f.removals != tt.wantRemovals {
				t.Errorf("label removals: got = %d, want = %d", f.removals, tt.wantRemovals)
			}
			if tt.wantUpdates > 0 || tt.wantRemovals > 0 {
				if h.agentCalls != 0 {
					t.Errorf("agent calls: got = %d, want = 0", h.agentCalls)
				}
			}
		})
	}
}

// TestReconcileIssueUpdateBranchRetry checks that a retry after an accepted
// update whose label removal failed does not update again: the branch now
// contains the default branch, so only the label comes off.
func TestReconcileIssueUpdateBranchRetry(t *testing.T) {
	h := newIssueHarness(t)
	h.openPR(t, "MERGEABLE", "[]")
	h.prLabels = []string{updateBranchLabel}
	fake := &fakeUpdateBranch{compareStatus: "diverged", updateCode: http.StatusAccepted, removeCode: http.StatusInternalServerError}
	h.extra = fake.serve
	opt := WithUpdateBranchLabel[*testRequest, *publishResult, struct{}](updateBranchLabel)
	agent := func(context.Context) error { return nil }

	if err := h.reconcileErr(t, t.Context(), agent, opt); err == nil {
		t.Fatal("first reconcile: got = nil, want = label removal error")
	}
	fake.compareStatus = "ahead"
	fake.removeCode = http.StatusOK
	if err := h.reconcileErr(t, t.Context(), agent, opt); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if fake.updates != 1 {
		t.Errorf("update calls: got = %d, want = 1", fake.updates)
	}
	if fake.removals != 2 {
		t.Errorf("label removals: got = %d, want = 2", fake.removals)
	}
}
