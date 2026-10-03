/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"text/template"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-github/v88/github"
)

// errBuildRequest stops a reconcile that gets as far as running the agent.
var errBuildRequest = errors.New("build request")

func TestReconcileIssueMergeabilityRecheck(t *testing.T) {
	tests := []struct {
		name        string
		recheck     time.Duration
		mergeable   []string // what each PR read returns; the last repeats
		pending     bool
		findings    bool
		maxCommits  int
		closeIssue  bool // the issue closes while the reconcile waits
		cancel      bool
		wantReads   int
		wantRequeue bool
		wantGreen   bool // the green path labels the PR ready for review
		wantErr     error
	}{{
		name:        "off by default",
		mergeable:   []string{"UNKNOWN", "MERGEABLE"},
		wantReads:   1,
		wantRequeue: true,
	}, {
		name:      "second read settles",
		recheck:   time.Millisecond,
		mergeable: []string{"UNKNOWN", "MERGEABLE"},
		wantReads: 2,
		wantGreen: true,
	}, {
		name:        "still unknown after second read",
		recheck:     time.Millisecond,
		mergeable:   []string{"UNKNOWN"},
		wantReads:   2,
		wantRequeue: true,
	}, {
		name:      "known on first read",
		recheck:   time.Millisecond,
		mergeable: []string{"MERGEABLE"},
		wantReads: 1,
		wantGreen: true,
	}, {
		name:      "pending checks decide first",
		recheck:   time.Millisecond,
		mergeable: []string{"UNKNOWN", "MERGEABLE"},
		pending:   true,
		wantReads: 1,
	}, {
		name:      "context ends during the wait",
		recheck:   time.Hour,
		mergeable: []string{"UNKNOWN", "MERGEABLE"},
		cancel:    true,
		wantReads: 1,
		wantErr:   context.Canceled,
	}, {
		name:      "findings decide first",
		recheck:   time.Millisecond,
		mergeable: []string{"UNKNOWN", "MERGEABLE"},
		findings:  true,
		wantReads: 1,
		wantErr:   errBuildRequest,
	}, {
		name:       "commit limit decides first",
		recheck:    time.Millisecond,
		mergeable:  []string{"UNKNOWN", "MERGEABLE"},
		maxCommits: 1,
		wantReads:  1,
	}, {
		name:       "issue closes during the wait",
		recheck:    time.Millisecond,
		mergeable:  []string{"UNKNOWN", "MERGEABLE"},
		closeIssue: true,
		wantReads:  2,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &github.Issue{
				Number:  new(123),
				HTMLURL: new("https://github.com/owner/repo/issues/123"),
				State:   new("open"),
				Labels:  []*github.Label{{Name: new("test-bot/managed")}},
			}
			checks := `[]`
			if tt.pending {
				checks = `[{"__typename":"CheckRun","databaseId":1,"name":"unit-tests","status":"IN_PROGRESS"}]`
			}
			if tt.findings {
				checks = `[{"__typename":"CheckRun","databaseId":1,"name":"unit-tests","status":"COMPLETED","conclusion":"FAILURE"}]`
			}
			cmOpts := []changemanager.Option[PRData[*testRequest]]{changemanager.WithFindingsIteration[PRData[*testRequest]]()}
			if tt.maxCommits > 0 {
				cmOpts = append(cmOpts, changemanager.WithMaxCommits[PRData[*testRequest]](tt.maxCommits))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var prReads, readyLabels int
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: publishTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read GraphQL body: got = %v, want = nil", err)
					}
					if !strings.Contains(string(body), "pullRequests(") {
						fmt.Fprint(w, `{"data":{}}`)
						return
					}
					mergeable := tt.mergeable[min(prReads, len(tt.mergeable)-1)]
					prReads++
					if tt.closeIssue {
						issue.State = new("closed")
					}
					if tt.cancel {
						// The first read is done, so the wait is what the cancel ends.
						cancel()
					}
					fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":456,"url":"https://github.com/owner/repo/pull/456","body":"","mergeable":%q,"isDraft":false,"headRefOid":"abc123","labels":{"nodes":[]},"commits":{"totalCount":1,"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":%s}}}}]},"assignees":{"nodes":[]},"reviewThreads":{"nodes":[]},"reviews":{"nodes":[]}}]}}}}`, mergeable, checks)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/123":
					if err := json.NewEncoder(w).Encode(issue); err != nil {
						t.Errorf("encode issue: got = %v, want = nil", err)
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read labels body: got = %v, want = nil", err)
					}
					if strings.Contains(string(body), "/ready-for-review") {
						readyLabels++
					}
					fmt.Fprint(w, `[]`)
				case r.Method == http.MethodGet:
					fmt.Fprint(w, `[]`)
				default:
					fmt.Fprint(w, `{}`)
				}
			})}}))
			if err != nil {
				t.Fatalf("create GitHub client: got = %v, want = nil", err)
			}
			cm, err := changemanager.New[PRData[*testRequest]]("test-bot", template.Must(template.New("title").Parse("Update content")), template.Must(template.New("body").Parse("Resolve #123.")), cmOpts...)
			if err != nil {
				t.Fatalf("create change manager: got = %v, want = nil", err)
			}
			opts := []Option[*testRequest, *publishResult, struct{}]{
				WithRequiredLabel[*testRequest, *publishResult, struct{}]("test-bot/managed"),
			}
			if tt.recheck > 0 {
				opts = append(opts, WithMergeabilityRecheck[*testRequest, *publishResult, struct{}](tt.recheck))
			}
			rec := New("test-bot", cm, nil, []string{"test-bot/managed"},
				&publishAgent{execute: func(context.Context) error {
					t.Error("agent ran: got = called, want = not called")
					return nil
				}},
				func(context.Context, *github.Issue, *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
					return nil, errBuildRequest
				},
				func(context.Context, *changemanager.Session[PRData[*testRequest]], *clonemanager.Lease) (struct{}, error) {
					return struct{}{}, nil
				},
				opts...,
			)
			err = rec.reconcileIssue(ctx, &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 123, Type: githubreconciler.ResourceTypeIssue}, gh)
			if _, _, requeue := workqueue.GetRequeueOptions(err); requeue != tt.wantRequeue {
				t.Errorf("requeue: got = %v (err %v), want = %v", requeue, err, tt.wantRequeue)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error: got = %v, want = %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !tt.wantRequeue && err != nil {
				t.Errorf("error: got = %v, want = nil", err)
			}
			if got := readyLabels > 0; got != tt.wantGreen {
				t.Errorf("ready-for-review label: got = %v, want = %v", got, tt.wantGreen)
			}
			if prReads != tt.wantReads {
				t.Errorf("PR reads: got = %d, want = %d", prReads, tt.wantReads)
			}
		})
	}
}
