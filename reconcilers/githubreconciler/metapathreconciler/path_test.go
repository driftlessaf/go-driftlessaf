/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
)

func TestPathExists(t *testing.T) {
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mod", "pkg"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mod", "go.mod"), []byte("module example.com/mod\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{name: "existing file", rel: "mod/go.mod", want: true},
		{name: "existing directory", rel: "mod/pkg", want: true},
		{name: "removed module", rel: "gone/go.mod", want: false},
		{name: "missing file in existing directory", rel: "mod/pkg/main.go", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pathExists(wt, tc.rel)
			if err != nil {
				t.Fatalf("pathExists(%q): unexpected error: %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("pathExists(%q): got = %v, want = %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestReconcilePathAddsResourceLabelsToExistingPR(t *testing.T) {
	initDefaultBranchRepo(t)
	resourceLabel := fmt.Sprintf("canary-%d", rand.Int64())
	var added [][]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":60,"url":"https://github.com/owner/repo/pull/60","mergeable":"MERGEABLE","headRefOid":"abc123","labels":{"nodes":[{"name":"automated pr"}]},"commits":{"totalCount":1,"nodes":[{"commit":{}}]}}]}}}}`)
	})
	mux.HandleFunc("POST /repos/owner/repo/issues/60/labels", func(w http.ResponseWriter, req *http.Request) {
		var labels []string
		if err := json.NewDecoder(req.Body).Decode(&labels); err != nil {
			t.Errorf("decode labels: %v", err)
		}
		added = append(added, labels)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	})
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerTransport{handler: mux}}))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	tmpl := template.Must(template.New("t").Parse("t"))
	cm, err := changemanager.New[PRData[*testRequest]]("test-identity", tmpl, tmpl)
	if err != nil {
		t.Fatalf("changemanager.New: %v", err)
	}
	var o prOptions
	WithLabels("automated pr").applyPR(&o)
	WithResourceLabels(func(*githubreconciler.Resource) []string { return []string{resourceLabel} }).applyPR(&o)
	r := &PRReconciler[*testRequest, *testResult, testCallbacks]{
		identity:        "test-identity",
		analyzer:        &recordingAnalyzer{},
		cloneMeta:       newTestCloneMeta(t),
		mode:            ModeFix,
		labels:          o.labels,
		resourceLabelFn: o.resourceLabelFn,
		changeManager:   cm,
	}

	// The PR has no checks, so it reads as green and the pass ends without
	// running the agent; the labels converge before that.
	if err := r.reconcilePath(t.Context(), pathResource("mod/go.mod"), gh); err != nil {
		t.Fatalf("reconcilePath: got = %v, want = nil", err)
	}
	if len(added) == 0 {
		t.Fatal("labels added to the PR: got = none, want = the resource label")
	}
	if diff := cmp.Diff([]string{resourceLabel}, added[0]); diff != "" {
		t.Errorf("labels added to the PR (-want, +got):\n%s", diff)
	}
}

func TestReconcilePathCommitLimit(t *testing.T) {
	const (
		gaveUpLabel = "test-identity/too-hard-need-human"
		readyLabel  = "test-identity/ready-for-review"
		limitLabel  = "test-identity/turn-limit"
		marker      = "<!--test-identity:no-changes-->"
		success     = `{"__typename":"CheckRun","databaseId":1,"name":"build","status":"COMPLETED","conclusion":"SUCCESS"}`
		pending     = `{"__typename":"CheckRun","databaseId":2,"name":"test","status":"IN_PROGRESS"}`
		failure     = `{"__typename":"CheckRun","databaseId":1,"name":"build","status":"COMPLETED","conclusion":"FAILURE"}`
	)
	for _, tc := range []struct {
		name       string
		commits    int
		checks     string
		wantLabels []string
		wantClear  bool
	}{
		{name: "green below limit", commits: 2, checks: success, wantLabels: []string{readyLabel}, wantClear: true},
		{name: "green at limit", commits: 3, checks: success, wantLabels: []string{readyLabel}, wantClear: true},
		{name: "green above limit", commits: 4, checks: success, wantLabels: []string{readyLabel}, wantClear: true},
		{name: "pending at limit", commits: 3, checks: pending},
		{name: "failed at limit", commits: 3, checks: failure, wantLabels: []string{limitLabel}},
		{name: "failed and pending at limit", commits: 3, checks: failure + "," + pending, wantLabels: []string{limitLabel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var added []string
			var removed []string
			var deleted []int64
			mux := http.NewServeMux()
			mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":60,"url":"https://github.com/owner/repo/pull/60","mergeable":"MERGEABLE","headRefOid":"abc123","labels":{"nodes":[{"name":%q}]},"commits":{"totalCount":%d,"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[%s]}}}}]}}]}}}}`, gaveUpLabel, tc.commits, tc.checks)
			})
			mux.HandleFunc("POST /repos/owner/repo/issues/60/labels", func(w http.ResponseWriter, req *http.Request) {
				var labels []string
				if err := json.NewDecoder(req.Body).Decode(&labels); err != nil {
					t.Fatalf("decode labels: %v", err)
				}
				added = append(added, labels...)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `[]`)
			})
			mux.HandleFunc("DELETE /repos/owner/repo/issues/60/labels/{label...}", func(w http.ResponseWriter, req *http.Request) {
				removed = append(removed, req.PathValue("label"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `[]`)
			})
			mux.HandleFunc("GET /repos/owner/repo/issues/60/comments", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `[{"id":42,"body":%q}]`, marker+"\nblocked on a dependency")
			})
			mux.HandleFunc("DELETE /repos/owner/repo/issues/comments/42", func(w http.ResponseWriter, _ *http.Request) {
				deleted = append(deleted, 42)
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
				t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				http.Error(w, "unexpected request", http.StatusNotFound)
			})
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerTransport{handler: mux}}))
			if err != nil {
				t.Fatalf("github.NewClient: %v", err)
			}
			tmpl := template.Must(template.New("t").Parse("t"))
			cm, err := changemanager.New[PRData[*testRequest]]("test-identity", tmpl, tmpl,
				changemanager.WithMaxCommits[PRData[*testRequest]](3),
				changemanager.WithFindingsIteration[PRData[*testRequest]](),
			)
			if err != nil {
				t.Fatalf("changemanager.New: %v", err)
			}
			// Every case must stop before leasing a clone or running the agent.
			r := &PRReconciler[*testRequest, *testResult, testCallbacks]{
				identity:      "test-identity",
				changeManager: cm,
				giveUp:        &changemanager.GiveUpComment{Marker: marker},
			}
			if err := r.reconcilePath(t.Context(), pathResource("mod/go.mod"), gh); err != nil {
				t.Fatalf("reconcilePath: got = %v, want = nil", err)
			}
			if diff := cmp.Diff(tc.wantLabels, added); diff != "" {
				t.Errorf("added labels (-want, +got):\n%s", diff)
			}
			var wantRemoved []string
			var wantDeleted []int64
			if tc.wantClear {
				wantRemoved = []string{gaveUpLabel}
				wantDeleted = []int64{42}
			}
			if diff := cmp.Diff(wantRemoved, removed); diff != "" {
				t.Errorf("removed labels (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantDeleted, deleted); diff != "" {
				t.Errorf("deleted comments (-want, +got):\n%s", diff)
			}
		})
	}
}
