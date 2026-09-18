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
	"math/rand/v2"
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
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

type publishResult struct{}

func (*publishResult) GetCommitMessage() string { return "Update content" }

// Keep an explanation present so an inactive issue incorrectly routed through
// ErrNoChanges would attempt to post a give-up comment.
func (*publishResult) GetNoChangeExplanation() string { return "Nothing remains to do." }

type publishAgent struct {
	execute func(context.Context) error
}

func (a *publishAgent) Execute(ctx context.Context, _ *testRequest, _ struct{}) (*publishResult, error) {
	if err := a.execute(ctx); err != nil {
		return nil, err
	}
	return &publishResult{}, nil
}

// publishTransport serves both REST and GraphQL through the same handler,
// including GraphQL requests whose URL is fixed to api.github.com.
type publishTransport struct{ handler http.Handler }

func (p publishTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	p.handler.ServeHTTP(recorder, req)
	return recorder.Result(), nil
}

func TestReconcileIssueRevalidatesBeforePublish(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		labels        []string
		refreshFails  bool
		noChanges     bool
		wantPublished bool
		wantErr       bool
		wantReads     int
		wantComments  int
	}{{
		name:          "open managed issue publishes",
		state:         "open",
		labels:        []string{"test-bot/managed"},
		wantPublished: true,
		wantReads:     2,
	}, {
		name:      "issue closes during agent run",
		state:     "closed",
		labels:    []string{"test-bot/managed"},
		wantReads: 2,
	}, {
		name:      "required label removed during agent run",
		state:     "open",
		wantReads: 2,
	}, {
		name:      "skip label added during agent run",
		state:     "open",
		labels:    []string{"test-bot/managed", "skip:test-bot"},
		wantReads: 2,
	}, {
		name:         "refresh failure prevents publication",
		state:        "open",
		labels:       []string{"test-bot/managed"},
		refreshFails: true,
		wantErr:      true,
		wantReads:    2,
	}, {
		name:         "clean worktree avoids refresh",
		state:        "open",
		labels:       []string{"test-bot/managed"},
		noChanges:    true,
		wantReads:    1,
		wantComments: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// SetRepoURLForTesting is process-global, so these cases run serially.
			t.Setenv("TMPDIR", t.TempDir())
			remoteDir, remote := publishRepository(t)
			t.Cleanup(clonemanager.SetRepoURLForTesting(func(*githubreconciler.Resource) string { return remoteDir }))
			issue := &github.Issue{
				Number:  new(123),
				HTMLURL: new("https://github.com/owner/repo/issues/123"),
				State:   new("open"),
				Labels:  []*github.Label{{Name: new("test-bot/managed")}},
			}
			var agentCalls, issueReads, prCreates, commentWrites, otherWrites int
			gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: publishTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					fmt.Fprint(w, `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/123":
					issueReads++
					if agentCalls > 0 && tt.refreshFails {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprint(w, `{"message":"refresh unavailable"}`)
						return
					}
					if err := json.NewEncoder(w).Encode(issue); err != nil {
						t.Errorf("encode issue: got = %v, want = nil", err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/compare/main...test-bot/issue-123":
					fmt.Fprint(w, `{"files":[{"filename":"content.txt"}]}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
					prCreates++
					fmt.Fprint(w, `{"number":456,"html_url":"https://github.com/owner/repo/pull/456"}`)
				case r.Method == http.MethodGet && (r.URL.Path == "/repos/owner/repo/issues/123/comments" || r.URL.Path == "/repos/owner/repo/issues/456/comments"):
					fmt.Fprint(w, `[]`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/123/comments":
					commentWrites++
					fmt.Fprint(w, `{"id":789}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/456/labels":
					otherWrites++
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("GitHub request: got = %s %s, want = supported request", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})}}))
			if err != nil {
				t.Fatalf("create GitHub client: got = %v, want = nil", err)
			}
			cm, err := changemanager.New[PRData[*testRequest]]("test-bot", template.Must(template.New("title").Parse("Update content")), template.Must(template.New("body").Parse("Resolve #123.")))
			if err != nil {
				t.Fatalf("create change manager: got = %v, want = nil", err)
			}
			content := fmt.Sprintf("content-%d\n", rand.Uint64())
			rec := New("test-bot", cm,
				clonemanager.NewMeta(t.Context(), func(context.Context, string, string) (oauth2.TokenSource, error) {
					return clonemanager.StaticTokenSource(""), nil
				}, "test-bot", nil),
				[]string{"test-bot/managed"},
				&publishAgent{execute: func(ctx context.Context) error {
					agentCalls++
					issue.State = new(tt.state)
					issue.Labels = make([]*github.Label, 0, len(tt.labels))
					for _, label := range tt.labels {
						issue.Labels = append(issue.Labels, &github.Label{Name: new(label)})
					}
					if tt.noChanges {
						return nil
					}
					wt, ok := clonemanager.WorktreeFromContext(ctx)
					if !ok {
						return errors.New("missing leased worktree")
					}
					return os.WriteFile(filepath.Join(wt.Filesystem.Root(), "content.txt"), []byte(content), 0o644)
				}},
				func(context.Context, *github.Issue, *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
					return &testRequest{}, nil
				},
				func(context.Context, *changemanager.Session[PRData[*testRequest]], *clonemanager.Lease) (struct{}, error) {
					return struct{}{}, nil
				},
				WithRequiredLabel[*testRequest, *publishResult, struct{}]("test-bot/managed"),
				WithGiveUpComment[*testRequest, *publishResult, struct{}]("<!--test:no-changes-->", func(explanation string) string { return explanation }),
			)
			err = rec.reconcileIssue(t.Context(), &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 123, Type: githubreconciler.ResourceTypeIssue}, gh)
			if (err != nil) != tt.wantErr {
				t.Fatalf("reconcile error: got = %v, want error = %v", err, tt.wantErr)
			}
			if agentCalls != 1 {
				t.Errorf("agent calls: got = %d, want = 1", agentCalls)
			}
			if issueReads != tt.wantReads {
				t.Errorf("issue reads: got = %d, want = %d", issueReads, tt.wantReads)
			}
			if commentWrites != tt.wantComments {
				t.Errorf("comment writes: got = %d, want = %d", commentWrites, tt.wantComments)
			}
			wantPRs := 0
			if tt.wantPublished {
				wantPRs = 1
			}
			if prCreates != wantPRs || otherWrites != wantPRs {
				t.Errorf("PR creates and label writes: got = %d and %d, want = %d each", prCreates, otherWrites, wantPRs)
			}
			ref, err := remote.Reference(plumbing.NewBranchReferenceName("test-bot/issue-123"), true)
			if !tt.wantPublished {
				if !errors.Is(err, plumbing.ErrReferenceNotFound) {
					t.Errorf("published branch: got = %v, %v, want = absent", ref, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("published branch: got = %v, want = nil", err)
			}
			commit, err := remote.CommitObject(ref.Hash())
			if err != nil {
				t.Fatalf("published commit: got = %v, want = nil", err)
			}
			file, err := commit.File("content.txt")
			if err != nil {
				t.Fatalf("published file: got = %v, want = nil", err)
			}
			if got, err := file.Contents(); err != nil || got != content {
				t.Errorf("published content: got = %q, %v, want = %q, nil", got, err, content)
			}
		})
	}
}

func publishRepository(t *testing.T) (string, *git.Repository) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")})
	if err != nil {
		t.Fatalf("initialize repository: got = %v, want = nil", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("repository worktree: got = %v, want = nil", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write initial file: got = %v, want = nil", err)
	}
	if _, err := wt.Add("content.txt"); err != nil {
		t.Fatalf("stage initial file: got = %v, want = nil", err)
	}
	if _, err := wt.Commit("Initial content", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatalf("initial commit: got = %v, want = nil", err)
	}
	return dir, repo
}
