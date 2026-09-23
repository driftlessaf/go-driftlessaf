/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/google/go-github/v88/github"
)

func TestCountNonMergeCommits(t *testing.T) {
	requests := 0
	var cursors, queries []string
	got, err := countNonMergeCommits(t.Context(), newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				Cursor string `json:"cursor"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		cursors = append(cursors, body.Variables.Cursor)
		queries = append(queries, body.Query)

		requests++
		if requests == 2 {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"commits":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"commit":{"parents":{"totalCount":1}}}]}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"commits":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[{"commit":{"parents":{"totalCount":1}}},{"commit":{"parents":{"totalCount":2}}}]}}}}}`))
	})), "owner", "repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("non-merge commit count: got = %d, want = 2", got)
	}
	if requests != 2 {
		t.Fatalf("request count: got = %d, want = 2", requests)
	}
	if want := []string{"", "next"}; !slices.Equal(cursors, want) {
		t.Errorf("request cursors: got = %v, want = %v", cursors, want)
	}
	if !strings.Contains(queries[1], "commits(first: 100, after: $cursor)") {
		t.Errorf("second query: got = %q, want commits page with after cursor", queries[1])
	}
}

func TestCountNonMergeCommitsPaginationError(t *testing.T) {
	requests := 0
	got, err := countNonMergeCommits(t.Context(), newTestGraphQLClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"commits":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[{"commit":{"parents":{"totalCount":1}}}]}}}}}`))
	})), "owner", "repo", 42)
	if err == nil {
		t.Fatal("countNonMergeCommits() error: got = nil, want pagination error")
	}
	if got != 0 {
		t.Errorf("non-merge commit count on error: got = %d, want = 0", got)
	}
	if !strings.Contains(err.Error(), "listing pull request commits") {
		t.Errorf("error: got = %q, want listing pull request commits", err)
	}
}

func TestNewSessionMergeCommitBudget(t *testing.T) {
	tests := []struct {
		name              string
		maxCommits        int
		excludeMerges     bool
		wantCommitQueries int
		wantHitMax        bool
	}{{
		name:              "merge exclusion uses non-merge count",
		maxCommits:        3,
		excludeMerges:     true,
		wantCommitQueries: 1,
		wantHitMax:        false,
	}, {
		name:       "default uses total count",
		maxCommits: 3,
		wantHitMax: true,
	}, {
		name:          "unlimited budget skips merge query",
		excludeMerges: true,
		wantHitMax:    false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commitQueries := 0
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Query string `json:"query"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decoding request: %v", err)
				}
				switch {
				case strings.Contains(body.Query, "pullRequests("):
					_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"nodes":[{"number":42,"url":"https://github.com/owner/repo/pull/42","body":"","mergeable":"MERGEABLE","isDraft":false,"headRefOid":"abc123","labels":{"nodes":[]},"commits":{"totalCount":4,"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}]},"assignees":{"nodes":[]},"reviewThreads":{"nodes":[]},"reviews":{"nodes":[]}}]}}}}`))
				case strings.Contains(body.Query, "commits(first: 100)"):
					commitQueries++
					_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"commits":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"commit":{"parents":{"totalCount":1}}},{"commit":{"parents":{"totalCount":1}}},{"commit":{"parents":{"totalCount":2}}},{"commit":{"parents":{"totalCount":2}}}]}}}}}`))
				default:
					t.Fatalf("unexpected GraphQL query: %s", body.Query)
				}
			})}}))
			if err != nil {
				t.Fatalf("creating GitHub client: %v", err)
			}

			opts := []Option[testData]{WithMaxCommits[testData](tt.maxCommits)}
			if tt.excludeMerges {
				opts = append(opts, WithMergeCommitsExcludedFromBudget[testData]())
			}
			cm, err := New[testData]("test-bot",
				template.Must(template.New("title").Parse("update")),
				template.Must(template.New("body").Parse("update")),
				opts...,
			)
			if err != nil {
				t.Fatalf("New(): %v", err)
			}

			session, err := cm.NewSession(t.Context(), gh, &githubreconciler.Resource{
				Owner: "owner",
				Repo:  "repo",
				Ref:   "main",
				Path:  "packages/example.yaml",
				Type:  githubreconciler.ResourceTypePath,
			})
			if err != nil {
				t.Fatalf("NewSession(): %v", err)
			}
			if got := session.CommitCount(); got != 4 {
				t.Errorf("CommitCount(): got = %d, want = 4", got)
			}
			if got := session.State().HitMaxCommits(); got != tt.wantHitMax {
				t.Errorf("HitMaxCommits(): got = %v, want = %v", got, tt.wantHitMax)
			}
			if commitQueries != tt.wantCommitQueries {
				t.Errorf("commit query count: got = %d, want = %d", commitQueries, tt.wantCommitQueries)
			}
		})
	}
}

func TestMergeCommitExclusionRejectsDynamicBudget(t *testing.T) {
	_, err := New[testData]("test-bot",
		template.Must(template.New("title").Parse("update")),
		template.Must(template.New("body").Parse("update")),
		WithMergeCommitsExcludedFromBudget[testData](),
		WithDynamicCommitBudget[testData](),
	)
	if err == nil {
		t.Fatal("New() error: got = nil, want incompatible options error")
	}
}
