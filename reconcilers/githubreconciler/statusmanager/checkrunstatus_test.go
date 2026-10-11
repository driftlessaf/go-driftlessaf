/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statusmanager

import (
	"testing"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
)

func TestCheckRunStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status, conclusion string
		unreadable               bool
	}{
		{name: "no run"},
		{name: "waiting", status: "in_progress"},
		{name: "expired", status: "completed", conclusion: "stale"},
		{name: "unreadable summary", status: "completed", conclusion: "failure", unreadable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := skipManager[TestDetails](t, false)
			server := &skipServer{}
			if tc.status != "" {
				server.live = liveRun(t, sm, "waiting", &Status[TestDetails]{Status: "in_progress"})
				server.live.Status = new(tc.status)
				server.live.Conclusion = new(tc.conclusion)
				if tc.unreadable {
					server.live.Output.Summary = new("no embedded state")
				}
			}
			session := sm.NewSession(server.start(t), &githubreconciler.Resource{Owner: skipOwner, Repo: skipRepo, Type: githubreconciler.ResourceTypePullRequest}, skipSHA)
			if status, conclusion := session.CheckRunStatus(); status != "" || conclusion != "" {
				t.Fatalf("before read: got = %q/%q, want empty", status, conclusion)
			}
			observed, err := session.ObservedState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if status, conclusion := session.CheckRunStatus(); status != tc.status || conclusion != tc.conclusion {
				t.Errorf("after read: got = %q/%q, want = %q/%q", status, conclusion, tc.status, tc.conclusion)
			}
			if observed != nil && (observed.Status != "in_progress" || observed.Conclusion != "") {
				t.Errorf("embedded state changed: got = %q/%q, want in_progress/", observed.Status, observed.Conclusion)
			}
			if err := session.SetActualState(t.Context(), "approved", &Status[TestDetails]{Status: "completed", Conclusion: "success"}); err != nil {
				t.Fatal(err)
			}
			if status, conclusion := session.CheckRunStatus(); status != "completed" || conclusion != "success" {
				t.Errorf("after write: got = %q/%q, want completed/success", status, conclusion)
			}
		})
	}
}
