/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/google/go-github/v88/github"
)

func TestNewSessionCIChecks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		skipChecks bool
		permission bool
		wantErr    bool
	}{
		{name: "default reads CI results", permission: true},
		{name: "default preserves permission errors", wantErr: true},
		{name: "opt out needs no status permission", skipChecks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			updates := 0
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.HasPrefix(req.URL.Path, "/repos/owner/repo/compare/") && req.Method == http.MethodGet {
					fmt.Fprint(w, `{"files":[{"filename":"workflow.yaml"}]}`)
					return
				}
				if req.URL.Path == "/repos/owner/repo/pulls/60" && (req.Method == http.MethodGet || req.Method == http.MethodPatch) {
					if req.Method == http.MethodPatch {
						updates++
					}
					fmt.Fprint(w, `{"number":60,"html_url":"https://github.com/owner/repo/pull/60"}`)
					return
				}
				if req.URL.Path != "/graphql" {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				requests++
				var body struct {
					Query     string
					Variables struct{ IncludeChecks bool }
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(body.Query, "statusCheckRollup @include(if: $includeChecks)") {
					t.Fatalf("query lacks conditional CI selection: %s", body.Query)
				}
				if body.Variables.IncludeChecks == tc.skipChecks {
					t.Errorf("includeChecks: got = %v, want = %v", body.Variables.IncludeChecks, !tc.skipChecks)
				}
				if body.Variables.IncludeChecks && !tc.permission {
					fmt.Fprint(w, `{"errors":[{"message":"Resource not accessible by integration"}]}`)
					return
				}
				commit := `{}`
				if body.Variables.IncludeChecks {
					commit = `{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"build","status":"COMPLETED","conclusion":"FAILURE"},{"__typename":"CheckRun","databaseId":2,"name":"test","status":"IN_PROGRESS"}]}}}`
				}
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":60,"url":"https://github.com/owner/repo/pull/60","mergeable":"MERGEABLE","headRefOid":"abc123","commits":{"totalCount":2,"nodes":[{"commit":%s}]}}]}}}}`, commit)
			})}}))
			if err != nil {
				t.Fatal(err)
			}
			cm, err := New[testData]("test-bot", template.Must(template.New("title").Parse("update")), template.Must(template.New("body").Parse("update")), WithFindingsIteration[testData]())
			if err != nil {
				t.Fatal(err)
			}
			var opts []SessionOption
			if tc.skipChecks {
				opts = append(opts, WithoutGlobalStatus())
			}
			session, err := cm.NewSession(t.Context(), gh, &githubreconciler.Resource{Owner: "owner", Repo: "repo", Ref: "main", Path: "workflow.yaml", Type: githubreconciler.ResourceTypePath}, opts...)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") {
					t.Fatalf("NewSession(): got = %v, want permission error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if session.PRNumber() != 60 || session.CommitCount() != 2 {
				t.Errorf("PR metadata: got number = %d, commits = %d, want 60, 2", session.PRNumber(), session.CommitCount())
			}
			if got := session.State().HasFindings(); got == tc.skipChecks {
				t.Errorf("HasFindings(): got = %v, want = %v", got, !tc.skipChecks)
			}
			if got := session.State().HasPendingChecks(); got == tc.skipChecks {
				t.Errorf("HasPendingChecks(): got = %v, want = %v", got, !tc.skipChecks)
			}
			changes := 0
			prURL, err := session.Upsert(t.Context(), &testData{Version: "v2"}, false, nil, func(context.Context, string) error {
				changes++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if prURL != "https://github.com/owner/repo/pull/60" || changes != 1 || updates != 1 {
				t.Errorf("PR update: got URL = %q, changes = %d, updates = %d, want existing PR and one update", prURL, changes, updates)
			}
			if requests != 1 {
				t.Errorf("requests: got = %d, want = 1", requests)
			}
		})
	}
}
