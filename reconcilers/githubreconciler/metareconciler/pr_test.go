/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-github/v88/github"
)

// A PR event re-queues each linked issue that carries the required label at
// linkedIssuePriority, so the feedback is handled ahead of new issues.
func TestReconcilePullRequestQueuesLinkedIssuesAtPRPriority(t *testing.T) {
	managed := fmt.Sprintf("https://github.com/owner/repo/issues/%d", rand.IntN(1_000_000))
	unmanaged := fmt.Sprintf("https://github.com/owner/repo/issues/%d", 1_000_000+rand.IntN(1_000_000))
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: publishTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("GitHub request: got = %s %s, want = POST /graphql", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[`+
			`{"url":%q,"labels":{"nodes":[{"name":"test-identity/managed"}]}},`+
			`{"url":%q,"labels":{"nodes":[{"name":"bug"}]}}]}}}}}`, managed, unmanaged)
	})}}))
	if err != nil {
		t.Fatalf("github.NewClient: got = %v, want = nil", err)
	}

	rec := New[*testRequest, *testResult, testCallbacks](
		"test-identity",
		nil,
		nil,
		nil,
		&fakeAgent{},
		func(_ context.Context, _ *github.Issue, _ *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(_ context.Context, _ *changemanager.Session[PRData[*testRequest]], _ *clonemanager.Lease) (testCallbacks, error) {
			return testCallbacks{}, nil
		},
		WithRequiredLabel[*testRequest, *testResult, testCallbacks]("test-identity/managed"),
	)

	err = rec.reconcilePullRequest(t.Context(), &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 456}, gh)

	got := workqueue.GetQueueKeys(err)
	want := []workqueue.QueueKey{{Key: managed, Priority: linkedIssuePriority}}
	if !slices.Equal(got, want) {
		t.Errorf("queued keys: got = %+v, want = %+v", got, want)
	}
}
