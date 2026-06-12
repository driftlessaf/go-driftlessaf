/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v84/github"
)

func TestUpsert(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}/{{.Version}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}} to {{.Version}}"))

	tests := []struct {
		name                 string
		prNumber             int      // 0 = no existing PR (create path), >0 = existing PR (update path)
		labels               []string // desired labels passed to Upsert
		existingPRLabels     []string // labels already on the PR returned by GET /pulls
		makeChangesErr       error    // nil = success path; otherwise returned from the makeChanges callback
		managedLabels        []string // labels declared managed via WithManagedLabels
		wantPRNumber         int
		wantAssignable       bool     // whether AddAssignees should work after Upsert
		wantErrIs            error    // expected error sentinel for the no-changes path
		wantAddLabelsCalled  bool     // whether POST /labels should be called
		wantAddLabelsPayload []string // labels expected in the POST /labels body, if called
		wantRemovedLabels    []string // labels expected to be removed via DELETE /labels/{name}
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
		name:                "update existing PR preserves prNumber",
		prNumber:            99,
		labels:              []string{"automated-pr"},
		wantPRNumber:        99,
		wantAssignable:      true,
		wantAddLabelsCalled: true, // no existing labels, so missing "automated-pr" is added via POST
	}, {
		name:           "ErrNoChanges propagates as ErrNoChanges",
		prNumber:       0,
		makeChangesErr: ErrNoChanges,
		wantErrIs:      ErrNoChanges,
	}, {
		name:           "ErrNothingToCommit translates to ErrNoChanges",
		prNumber:       0,
		makeChangesErr: clonemanager.ErrNothingToCommit,
		wantErrIs:      ErrNoChanges,
	}, {
		name:           "wrapped ErrNothingToCommit translates to ErrNoChanges",
		prNumber:       0,
		makeChangesErr: fmt.Errorf("committing changes: %w", clonemanager.ErrNothingToCommit),
		wantErrIs:      ErrNoChanges,
	}, {
		name:                "update existing PR does not replace labels, preserves extra labels",
		prNumber:            99,
		labels:              []string{"automated-pr"},
		existingPRLabels:    []string{"automated-pr", "agentic", "bincapz/pass"},
		wantPRNumber:        99,
		wantAssignable:      true,
		wantAddLabelsCalled: false, // "automated-pr" is already present, so no add needed
	}, {
		name:                 "update existing PR adds only missing desired labels",
		prNumber:             99,
		labels:               []string{"automated-pr", "cve-remediation"},
		existingPRLabels:     []string{"automated-pr", "agentic"},
		wantPRNumber:         99,
		wantAssignable:       true,
		wantAddLabelsCalled:  true,                        // "cve-remediation" is missing
		wantAddLabelsPayload: []string{"cve-remediation"}, // only the missing one
	}, {
		name:              "update existing PR removes managed label no longer desired",
		prNumber:          99,
		labels:            []string{"automated-pr"},
		existingPRLabels:  []string{"automated-pr", "skip:approver-bot", "agentic"},
		managedLabels:     []string{"skip:approver-bot"},
		wantPRNumber:      99,
		wantAssignable:    true,
		wantRemovedLabels: []string{"skip:approver-bot"}, // managed, on PR, no longer desired
	}, {
		name:                 "update existing PR keeps managed label still desired",
		prNumber:             99,
		labels:               []string{"automated-pr", "skip:approver-bot"},
		existingPRLabels:     []string{"automated-pr"},
		managedLabels:        []string{"skip:approver-bot"},
		wantPRNumber:         99,
		wantAssignable:       true,
		wantAddLabelsCalled:  true,
		wantAddLabelsPayload: []string{"skip:approver-bot"}, // still desired and missing, so added
	}, {
		name:             "update existing PR never removes unmanaged label",
		prNumber:         99,
		labels:           []string{"automated-pr"},
		existingPRLabels: []string{"automated-pr", "skip:approver-bot"},
		managedLabels:    nil, // skip:approver-bot is not managed, so it stays
		wantPRNumber:     99,
		wantAssignable:   true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var addAssigneesCalled bool
			var replaceLabelsCalledCount int
			var addLabelsCalled bool
			var addLabelsPayload []string
			var removedLabels []string

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

			// Get PR (for update path skip-label check) — return existingPRLabels if set.
			mux.HandleFunc("GET /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
				ghLabels := make([]*github.Label, 0, len(tt.existingPRLabels))
				for _, name := range tt.existingPRLabels {
					n := name
					ghLabels = append(ghLabels, &github.Label{Name: &n})
				}
				writeJSON(t, w, &github.PullRequest{
					Number: github.Ptr(tt.prNumber),
					Labels: ghLabels,
				})
			})

			// Edit PR (update path)
			mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, &github.PullRequest{Number: github.Ptr(tt.prNumber)})
			})

			// Add labels (POST) — track calls and payload.
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
				addLabelsCalled = true
				var payload []string
				if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
					addLabelsPayload = payload
				}
				writeJSON(t, w, []*github.Label{})
			})

			// Replace labels (PUT) — track that this is never called on updates.
			mux.HandleFunc("PUT /api/v3/repos/test-owner/test-repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
				replaceLabelsCalledCount++
				writeJSON(t, w, []*github.Label{})
			})

			// Remove label (DELETE) — track which labels are removed.
			mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/issues/{number}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
				removedLabels = append(removedLabels, r.PathValue("name"))
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

			cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl, WithManagedLabels[testData](tt.managedLabels...))
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
				return tt.makeChangesErr
			})

			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Errorf("Upsert: got error = %v, wanted errors.Is(_, %v)", err, tt.wantErrIs)
				}
				return
			}

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

			// Verify label replace (PUT) was never called on the update path.
			if tt.prNumber > 0 && replaceLabelsCalledCount > 0 {
				t.Errorf("PUT /labels called %d time(s); labels must be added, not replaced", replaceLabelsCalledCount)
			}

			// Verify whether POST /labels was called and with the right payload.
			if tt.wantAddLabelsCalled && !addLabelsCalled {
				t.Error("POST /labels not called; expected missing labels to be added")
			}
			if !tt.wantAddLabelsCalled && addLabelsCalled && tt.prNumber > 0 {
				t.Errorf("POST /labels called unexpectedly with payload %v", addLabelsPayload)
			}
			if tt.wantAddLabelsPayload != nil {
				gotSet := make(map[string]struct{}, len(addLabelsPayload))
				for _, l := range addLabelsPayload {
					gotSet[l] = struct{}{}
				}
				for _, want := range tt.wantAddLabelsPayload {
					if _, ok := gotSet[want]; !ok {
						t.Errorf("POST /labels payload missing %q; got %v", want, addLabelsPayload)
					}
				}
				if len(addLabelsPayload) != len(tt.wantAddLabelsPayload) {
					t.Errorf("POST /labels payload length: got %d, want %d; payload=%v", len(addLabelsPayload), len(tt.wantAddLabelsPayload), addLabelsPayload)
				}
			}

			// Verify managed labels no longer desired were removed (and nothing else).
			if !slicesEqualUnordered(removedLabels, tt.wantRemovedLabels) {
				t.Errorf("DELETE /labels: got removed = %v, want = %v", removedLabels, tt.wantRemovedLabels)
			}
		})
	}
}

// TestUpsertEmbedsWrapper verifies Upsert persists one block with both the
// caller's data and the budget metadata, so an in-memory reset round-trips.
func TestUpsertEmbedsWrapper(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}}"))

	var createdBody string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		var pr github.NewPullRequest
		if err := json.NewDecoder(r.Body).Decode(&pr); err == nil {
			createdBody = pr.GetBody()
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, &github.PullRequest{Number: github.Ptr(7), HTMLURL: github.Ptr("https://example.test/pull/7")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := github.NewClient(nil).WithEnterpriseURLs(srv.URL, srv.URL)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl,
		WithDynamicCommitBudget[testData](), WithCloseOnEmptyDiff[testData](false))
	if err != nil {
		t.Fatalf("creating CM: %v", err)
	}

	// findingsMeta mimics a prior in-memory ResetCommitBudget; Upsert must carry it.
	session := &Session[testData]{
		manager:    cm,
		client:     client,
		owner:      "test-owner",
		repo:       "test-repo",
		branchName: "test-bot/pkg",
		ref:        "main",
		meta:       metadata{CommitBudgetBaseline: 9},
	}

	data := &testData{PackageName: "pkg"}
	if _, err := session.Upsert(t.Context(), data, false, nil, func(_ context.Context, _ string) error {
		return nil
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	ed, err := cm.templateExecutor.Extract(createdBody)
	if err != nil {
		t.Fatalf("extracting wrapper from created PR body: %v", err)
	}
	if ed.Data.PackageName != "pkg" {
		t.Errorf("embedded Data.PackageName: got = %q, want = %q", ed.Data.PackageName, "pkg")
	}
	if ed.Meta.CommitBudgetBaseline != 9 {
		t.Errorf("embedded baseline: got = %d, want = 9", ed.Meta.CommitBudgetBaseline)
	}
}

// slicesEqualUnordered reports whether a and b contain the same elements,
// ignoring order. Both nil and empty slices are treated as equal.
func slicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
		if counts[v] < 0 {
			return false
		}
	}
	return true
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encoding JSON response: %v", err)
	}
}
