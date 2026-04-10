/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"text/template"

	"github.com/google/go-github/v84/github"
)

func TestUpsert(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}/{{.Version}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}} to {{.Version}}"))

	tests := []struct {
		name           string
		prNumber       int // 0 = no existing PR (create path), >0 = existing PR (update path)
		labels         []string
		wantPRNumber   int
		wantAssignable bool // whether AddAssignees should work after Upsert
	}{{
		name:           "create new PR sets prNumber and prURL",
		prNumber:       0,
		labels:         []string{"automated-pr"},
		wantPRNumber:   42,
		wantAssignable: true,
	}, {
		name:           "create new PR without labels",
		prNumber:       0,
		wantPRNumber:   42,
		wantAssignable: true,
	}, {
		name:           "update existing PR preserves prNumber",
		prNumber:       99,
		labels:         []string{"automated-pr"},
		wantPRNumber:   99,
		wantAssignable: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var addAssigneesCalled bool

			mux := http.NewServeMux()

			// CompareCommits — always return at least one file so Upsert proceeds.
			mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/compare/{rest...}", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, &github.CommitsComparison{
					Files: []*github.CommitFile{{Filename: github.Ptr("README.md")}},
				})
			})

			// Create PR
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/pulls", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				writeJSON(t, w, &github.PullRequest{
					Number:  github.Ptr(42),
					HTMLURL: github.Ptr(fmt.Sprintf("https://%s/repos/test-owner/test-repo/pull/42", r.Host)),
				})
			})

			// Get PR (for update path skip-label check)
			mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, &github.PullRequest{
					Number: github.Ptr(tt.prNumber),
					Labels: []*github.Label{}, // no skip label
				})
			})

			// Edit PR (update path)
			mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, &github.PullRequest{Number: github.Ptr(tt.prNumber)})
			})

			// Add labels
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, []*github.Label{})
			})

			// Replace labels (update path)
			mux.HandleFunc("PUT /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, []*github.Label{})
			})

			// Add assignees — track whether this is called
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/assignees", func(w http.ResponseWriter, _ *http.Request) {
				addAssigneesCalled = true
				writeJSON(t, w, &github.Issue{})
			})

			srv := httptest.NewServer(mux)
			defer srv.Close()

			client, err := github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl)
			if err != nil {
				t.Fatalf("creating CM: %v", err)
			}

			prURL := ""
			if tt.prNumber > 0 {
				prURL = fmt.Sprintf("%s/repos/test-owner/test-repo/pull/%d", srv.URL, tt.prNumber)
			}

			session := &Session[testData]{
				manager:    cm,
				client:     client,
				owner:      "test-owner",
				repo:       "test-repo",
				branchName: "test-bot/issue-1",
				ref:        "main",
				prNumber:   tt.prNumber,
				prURL:      prURL,
			}

			data := &testData{
				PackageName: fmt.Sprintf("pkg-%d", rand.Int64()),
				Version:     fmt.Sprintf("v%d.%d.%d", rand.IntN(10), rand.IntN(10), rand.IntN(10)),
				Commit:      fmt.Sprintf("abc%d", rand.Int64()),
			}

			gotURL, err := session.Upsert(t.Context(), data, false, tt.labels, func(_ context.Context, _ string) error {
				return nil // no-op: pretend we made changes
			})
			if err != nil {
				t.Fatalf("Upsert: got error = %v, wanted nil", err)
			}

			// Verify prNumber is set on the session.
			if session.prNumber != tt.wantPRNumber {
				t.Errorf("session.prNumber: got = %d, wanted = %d", session.prNumber, tt.wantPRNumber)
			}

			// Verify prURL is set and returned.
			if session.prURL == "" {
				t.Error("session.prURL: got empty string, wanted non-empty")
			}
			if gotURL != session.prURL {
				t.Errorf("Upsert return value: got = %q, wanted = %q", gotURL, session.prURL)
			}

			// Verify AddAssignees works after Upsert (the bug was that it silently
			// no-op'd because prNumber was 0 after creating a new PR).
			if tt.wantAssignable {
				if err := session.AddAssignees(t.Context(), []string{"jdolitsky"}); err != nil {
					t.Fatalf("AddAssignees: got error = %v, wanted nil", err)
				}
				if !addAssigneesCalled {
					t.Error("AddAssignees: GitHub API was not called, wanted it to be called")
				}
			}
		})
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encoding JSON response: %v", err)
	}
}
