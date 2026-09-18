/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"text/template"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/issuemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

func TestWithSyntheticPaths(t *testing.T) {
	var pr prOptions
	WithSyntheticPaths().applyPR(&pr)
	if !pr.syntheticPaths {
		t.Error("applyPR: syntheticPaths = false, want true")
	}

	var issues issuesOptions
	WithSyntheticPaths().applyIssues(&issues)
	if !issues.syntheticPaths {
		t.Error("applyIssues: syntheticPaths = false, want true")
	}
}

// errAnalyzerReached ends a pass at the analyzer, before any git or GitHub
// write, so a test can tell whether the path leg reached analysis.
var errAnalyzerReached = errors.New("analyzer reached")

// recordingAnalyzer records the paths handed to Analyze and fails the pass.
type recordingAnalyzer struct {
	paths [][]string
}

func (a *recordingAnalyzer) Analyze(_ context.Context, _ *gogit.Worktree, paths []string, _ ...Diagnostic) ([]Diagnostic, error) {
	a.paths = append(a.paths, paths)
	return nil, errAnalyzerReached
}

// missingPathCases drives a path leg with a file the default branch carries,
// a file it no longer carries, and a path that names no file at all. Only a
// reconciler that declares synthetic paths analyzes the last one; every other
// missing path completes the key without analysis.
var missingPathCases = []struct {
	name           string
	path           string
	syntheticPaths bool
	wantAnalyzed   bool
}{
	{name: "existing file is analyzed", path: "mod/go.mod", wantAnalyzed: true},
	{name: "removed file completes the key without analysis", path: "gone/go.mod"},
	{name: "synthetic path is analyzed when declared", path: "mod-finding-1", syntheticPaths: true, wantAnalyzed: true},
	{name: "synthetic path reads as removed when not declared", path: "mod-finding-1"},
}

// checkAnalyzed asserts that the pass reached the analyzer exactly when the
// case expects, with the resource path passed through unchanged.
func checkAnalyzed(t *testing.T, analyzer *recordingAnalyzer, path string, err error, wantAnalyzed bool) {
	t.Helper()
	switch {
	case wantAnalyzed && !errors.Is(err, errAnalyzerReached):
		t.Fatalf("reconcile %q: got err = %v, want the analyzer to run", path, err)
	case !wantAnalyzed && err != nil:
		t.Fatalf("reconcile %q: got err = %v, want the key to complete", path, err)
	}
	if analyzed := len(analyzer.paths) > 0; analyzed != wantAnalyzed {
		t.Fatalf("reconcile %q: analyzer ran = %v, want %v", path, analyzed, wantAnalyzed)
	}
	if wantAnalyzed && (len(analyzer.paths[0]) != 1 || analyzer.paths[0][0] != path) {
		t.Errorf("reconcile %q: analyzer paths = %v, want [[%q]]", path, analyzer.paths, path)
	}
}

func TestReconcilePathMissingPath(t *testing.T) {
	initDefaultBranchRepo(t)
	gh := newEmptyRepoGitHubClient(t)
	cloneMeta := newTestCloneMeta(t)
	tmpl := template.Must(template.New("t").Parse("t"))
	cm, err := changemanager.New[PRData[*testRequest]]("test-identity", tmpl, tmpl)
	if err != nil {
		t.Fatalf("changemanager.New: %v", err)
	}

	for _, tc := range missingPathCases {
		t.Run(tc.name, func(t *testing.T) {
			analyzer := &recordingAnalyzer{}
			r := &PRReconciler[*testRequest, *testResult, testCallbacks]{
				identity:       "test-identity",
				analyzer:       analyzer,
				cloneMeta:      cloneMeta,
				mode:           ModeFix,
				syntheticPaths: tc.syntheticPaths,
				changeManager:  cm,
			}
			err := r.reconcilePath(t.Context(), pathResource(tc.path), gh)
			checkAnalyzed(t, analyzer, tc.path, err, tc.wantAnalyzed)
		})
	}
}

func TestReconcilePathIssuesMissingPath(t *testing.T) {
	initDefaultBranchRepo(t)
	gh := newEmptyRepoGitHubClient(t)
	cloneMeta := newTestCloneMeta(t)
	tmpl := template.Must(template.New("t").Parse("t"))
	im, err := issuemanager.New[IssueData]("test-identity", tmpl, tmpl)
	if err != nil {
		t.Fatalf("issuemanager.New: %v", err)
	}

	for _, tc := range missingPathCases {
		t.Run(tc.name, func(t *testing.T) {
			analyzer := &recordingAnalyzer{}
			r := &IssueReconciler{
				identity:       "test-identity",
				analyzer:       analyzer,
				cloneMeta:      cloneMeta,
				mode:           ModeFix,
				syntheticPaths: tc.syntheticPaths,
				issueManager:   im,
				grouping:       GroupByRule,
			}
			err := r.reconcilePathIssues(t.Context(), pathResource(tc.path), gh)
			checkAnalyzed(t, analyzer, tc.path, err, tc.wantAnalyzed)
		})
	}
}

func pathResource(path string) *githubreconciler.Resource {
	return &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: "owner",
		Repo:  "repo",
		Path:  path,
		Ref:   "main",
	}
}

// hermeticGit keeps every git lookup inside the repositories under test for
// the duration of a test. The clone manager spawns git to clone and fetch, and
// go-git reads the global config while committing, so a developer's URL
// rewrites, credential helper, or signing setup could otherwise reach the
// test. HOME and XDG point at empty temp dirs, both config variables at the
// null device, the identity is fixed, and a credential prompt fails fast
// instead of hanging.
func hermeticGit(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// initDefaultBranchRepo creates a repository whose main branch holds
// mod/go.mod and redirects clonemanager leases to it for the test's duration.
func initDefaultBranchRepo(t *testing.T) {
	t.Helper()
	hermeticGit(t)
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mod"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mod", "go.mod"), []byte("module example.com/mod\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add("mod/go.mod"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Cleanup(clonemanager.SetRepoURLForTesting(func(*githubreconciler.Resource) string { return dir }))
}

func newTestCloneMeta(t *testing.T) *clonemanager.Meta {
	t.Helper()
	return clonemanager.NewMeta(t.Context(), func(context.Context, string, string) (oauth2.TokenSource, error) {
		return clonemanager.StaticTokenSource(""), nil
	}, "test-identity", nil)
}

// handlerTransport serves every request, REST and GraphQL alike, from handler.
type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// newEmptyRepoGitHubClient returns a client for a repository with no open pull
// request or issue; any other request fails the pass with a 404.
func newEmptyRepoGitHubClient(t *testing.T) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	})
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: handlerTransport{handler: mux}}))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	return client
}
