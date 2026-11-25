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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// headlineAgent commits a distinct file change on every run, under whatever
// commit message the test sets before the run.
type headlineAgent struct {
	commitMsg string
	runs      int
}

func (a *headlineAgent) Execute(ctx context.Context, _ *testRequest, _ struct{}) (*testResult, error) {
	a.runs++
	wt, ok := clonemanager.WorktreeFromContext(ctx)
	if !ok {
		return nil, errors.New("missing leased worktree")
	}
	content := fmt.Sprintf("run-%d\n", a.runs)
	if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "content.txt"), []byte(content), 0o644); err != nil {
		return nil, err
	}
	return &testResult{commitMsg: a.commitMsg}, nil
}

// The PR title becomes the squash commit's subject on the default branch, so
// it has to name the change the PR makes. A CI fix-up commit stacked on the
// PR branch names only the fix-up; a rebuild from the default branch replaces
// the change and so names it afresh, for itself and for the fix-ups stacked on
// it.
func TestReconcileIssuePRTitleNamesTheChange(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	remoteDir, _ := publishRepository(t)
	t.Cleanup(clonemanager.SetRepoURLForTesting(func(*githubreconciler.Resource) string { return remoteDir }))

	issue := &github.Issue{
		Number:  new(123),
		HTMLURL: new("https://github.com/owner/repo/issues/123"),
		State:   new("open"),
		Labels:  []*github.Label{{Name: new("test-bot/managed")}},
	}

	// pr is the fake GitHub's view of the bot's pull request; the zero value
	// is no pull request.
	var pr struct {
		exists    bool
		title     string
		body      string
		mergeable string
		checks    string
		commits   int
	}
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: publishTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read GraphQL request: got = %v, want = nil", err)
			}
			switch {
			case strings.Contains(string(body), "pullRequests(") && !pr.exists:
				fmt.Fprint(w, `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`)
			case strings.Contains(string(body), "pullRequests("):
				prBody, err := json.Marshal(pr.body)
				if err != nil {
					t.Errorf("encode PR body: got = %v, want = nil", err)
				}
				fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"nodes":[{"number":456,"url":"https://github.com/owner/repo/pull/456","body":%s,"mergeable":%q,"isDraft":false,"headRefOid":"abc123","labels":{"nodes":[]},"commits":{"totalCount":%d,"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":%s}}}}]},"assignees":{"nodes":[]},"reviewThreads":{"nodes":[]},"reviews":{"nodes":[]}}]}}}}`, prBody, pr.mergeable, pr.commits, pr.checks)
			case strings.Contains(string(body), "timelineItems("):
				fmt.Fprint(w, `{"data":{"repository":{"issue":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}},"timelineItems":{"nodes":[]}}}}}`)
			default:
				fmt.Fprint(w, `{"data":{}}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/123":
			if err := json.NewEncoder(w).Encode(issue); err != nil {
				t.Errorf("encode issue: got = %v, want = nil", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/compare/main...test-bot/issue-123":
			fmt.Fprint(w, `{"files":[{"filename":"content.txt"}]}`)
		case (r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls") ||
			(r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/pulls/456"):
			var edit struct{ Title, Body string }
			if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
				t.Errorf("decode PR write: got = %v, want = nil", err)
			}
			pr.exists, pr.title, pr.body = true, edit.Title, edit.Body
			fmt.Fprint(w, `{"number":456,"html_url":"https://github.com/owner/repo/pull/456"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/456":
			fmt.Fprint(w, `{"number":456}`)
		case r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/labels"):
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})}}))
	if err != nil {
		t.Fatalf("create GitHub client: got = %v, want = nil", err)
	}

	cm, err := changemanager.New[PRData[*testRequest]]("test-bot",
		template.Must(template.New("title").Parse(`{{if .Headline}}{{.Headline}} (#{{.IssueNumber}}){{else}}fallback (#{{.IssueNumber}}){{end}}`)),
		template.Must(template.New("body").Parse("Resolve #123.")),
		changemanager.WithFindingsIteration[PRData[*testRequest]]())
	if err != nil {
		t.Fatalf("create change manager: got = %v, want = nil", err)
	}
	agent := &headlineAgent{}
	rec := New("test-bot", cm,
		clonemanager.NewMeta(t.Context(), func(context.Context, string, string) (oauth2.TokenSource, error) {
			return clonemanager.StaticTokenSource(""), nil
		}, "test-bot", nil),
		[]string{"test-bot/managed"},
		agent,
		func(context.Context, *github.Issue, *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(context.Context, *changemanager.Session[PRData[*testRequest]], *clonemanager.Lease) (struct{}, error) {
			return struct{}{}, nil
		},
		WithRequiredLabel[*testRequest, *testResult, struct{}]("test-bot/managed"),
		WithPRRenderFromResult[*testRequest, *testResult, struct{}](func(res *testResult) PRRender {
			headline, _, _ := strings.Cut(res.GetCommitMessage(), "\n")
			return PRRender{Headline: headline}
		}),
	)
	reconcile := func(commitMsg string) string {
		t.Helper()
		agent.commitMsg = commitMsg
		if err := rec.reconcileIssue(t.Context(), &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 123, Type: githubreconciler.ResourceTypeIssue}, gh); err != nil {
			t.Fatalf("reconcile: got = %v, want = nil", err)
		}
		pr.commits++
		return pr.title
	}

	if got, want := reconcile("feat(widget): add the widget\n\nBody."), "feat(widget): add the widget (#123)"; got != want {
		t.Errorf("title after the first run: got = %q, want = %q", got, want)
	}

	pr.mergeable = "MERGEABLE"
	pr.checks = `[{"__typename":"CheckRun","databaseId":1,"name":"unit-tests","status":"COMPLETED","conclusion":"FAILURE"}]`
	if got, want := reconcile("chore: fix copyright year"), "feat(widget): add the widget (#123)"; got != want {
		t.Errorf("title after a fix-up on the PR branch: got = %q, want = %q", got, want)
	}

	pr.mergeable = "CONFLICTING"
	pr.checks = `[]`
	if got, want := reconcile("feat(widget): add the widget on the new base"), "feat(widget): add the widget on the new base (#123)"; got != want {
		t.Errorf("title after a rebuild from the default branch: got = %q, want = %q", got, want)
	}

	// The rebuild discarded the first run's change, so a fix-up stacked on the
	// rebuilt branch keeps the rebuild's headline, not the first run's.
	pr.mergeable = "MERGEABLE"
	pr.checks = `[{"__typename":"CheckRun","databaseId":1,"name":"unit-tests","status":"COMPLETED","conclusion":"FAILURE"}]`
	if got, want := reconcile("chore: fix lint"), "feat(widget): add the widget on the new base (#123)"; got != want {
		t.Errorf("title after a fix-up on the rebuilt branch: got = %q, want = %q", got, want)
	}

	if agent.runs != 4 {
		t.Errorf("agent runs: got = %d, want = 4", agent.runs)
	}
}
