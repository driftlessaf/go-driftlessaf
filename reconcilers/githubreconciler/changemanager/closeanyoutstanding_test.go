/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-github/v88/github"
)

// TestCloseAnyOutstanding verifies that closing a PR deletes its head branch,
// and that the branch survives when the manager opts out, the closed PR
// carries the skip label, or its head is not this session's branch in this
// repository. A failed delete does not fail the close, and a reconcile
// canceled after the close still sends the delete.
func TestCloseAnyOutstanding(t *testing.T) {
	titleTmpl := template.Must(template.New("title").Parse("{{.PackageName}}"))
	bodyTmpl := template.Must(template.New("body").Parse("Update {{.PackageName}}"))

	const branch = "test-bot/packages/_dot_hidden/foo.yaml"

	tests := []struct {
		name             string
		cmOpts           []Option[testData]
		noPR             bool
		message          string
		closedPR         *github.PullRequest // PATCH response; nil fails the close
		cancelAfterClose bool                // cancel the reconcile's context once the close returns
		failDelete       bool
		wantErr          string
		wantComment      string
		wantClosed       bool
		wantDeleted      []string
	}{{
		name:        "closes the PR and deletes its branch",
		message:     "Closing PR because the issue was closed.",
		closedPR:    closedPR(branch, "test-owner/test-repo"),
		wantComment: "Closing PR because the issue was closed.",
		wantClosed:  true,
		wantDeleted: []string{"heads/" + branch},
	}, {
		name:        "empty message closes without a comment",
		closedPR:    closedPR(branch, "test-owner/test-repo"),
		wantClosed:  true,
		wantDeleted: []string{"heads/" + branch},
	}, {
		name:        "head repository matches case-insensitively",
		closedPR:    closedPR(branch, "Test-Owner/Test-Repo"),
		wantClosed:  true,
		wantDeleted: []string{"heads/" + branch},
	}, {
		name:       "WithDeleteBranchOnClose(false) keeps the branch",
		cmOpts:     []Option[testData]{WithDeleteBranchOnClose[testData](false)},
		closedPR:   closedPR(branch, "test-owner/test-repo"),
		wantClosed: true,
	}, {
		name:       "skip label on the closed PR keeps the branch",
		closedPR:   closedPR(branch, "test-owner/test-repo", "skip:test-bot"),
		wantClosed: true,
	}, {
		name:        "another identity's skip label does not keep the branch",
		closedPR:    closedPR(branch, "test-owner/test-repo", "skip:other-bot"),
		wantClosed:  true,
		wantDeleted: []string{"heads/" + branch},
	}, {
		name:       "head on a fork keeps the same-named branch here",
		closedPR:   closedPR(branch, "someone/test-repo"),
		wantClosed: true,
	}, {
		name:       "head ref that is not the session branch is kept",
		closedPR:   closedPR("test-bot/other", "test-owner/test-repo"),
		wantClosed: true,
	}, {
		name:       "response without a head deletes nothing",
		closedPR:   &github.PullRequest{Number: new(99), State: new("closed")},
		wantClosed: true,
	}, {
		name:             "reconcile canceled after the close still deletes the branch",
		closedPR:         closedPR(branch, "test-owner/test-repo"),
		cancelAfterClose: true,
		wantClosed:       true,
		wantDeleted:      []string{"heads/" + branch},
	}, {
		name:        "failed delete still reports the close as successful",
		closedPR:    closedPR(branch, "test-owner/test-repo"),
		failDelete:  true,
		wantClosed:  true,
		wantDeleted: []string{"heads/" + branch},
	}, {
		name:    "failed close returns an error and deletes nothing",
		wantErr: "closing pull request",
	}, {
		name: "no PR is a no-op",
		noPR: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				comments []string
				closed   bool
				deleted  []string
			)

			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
				var c github.IssueComment
				if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
					t.Errorf("decoding comment: %v", err)
				}
				comments = append(comments, c.GetBody())
				writeJSON(t, w, &c)
			})
			mux.HandleFunc("PATCH /api/v3/repos/test-owner/test-repo/pulls/99", func(w http.ResponseWriter, r *http.Request) {
				var pr github.PullRequest
				if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
					t.Errorf("decoding PR edit: %v", err)
				}
				if pr.GetState() != "closed" {
					t.Errorf("PR edit state: got = %q, wanted = %q", pr.GetState(), "closed")
				}
				if tt.closedPR == nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				closed = true
				writeJSON(t, w, tt.closedPR)
			})
			mux.HandleFunc("DELETE /api/v3/repos/test-owner/test-repo/git/refs/{ref...}", func(w http.ResponseWriter, r *http.Request) {
				deleted = append(deleted, r.PathValue("ref"))
				if tt.failDelete {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clientOpts := []github.ClientOptionsFunc{github.WithEnterpriseURLs(srv.URL, srv.URL)}
			if tt.cancelAfterClose {
				clientOpts = append(clientOpts, github.WithTransport(&cancelAfterClose{next: http.DefaultTransport, cancel: cancel}))
			}
			client, err := github.NewClient(clientOpts...)
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			cm, err := New[testData]("test-bot", titleTmpl, bodyTmpl, tt.cmOpts...)
			if err != nil {
				t.Fatalf("creating CM: %v", err)
			}

			prNumber := 99
			if tt.noPR {
				prNumber = 0
			}
			session := &Session[testData]{
				manager:    cm,
				client:     client,
				owner:      "test-owner",
				repo:       "test-repo",
				branchName: branch,
				prNumber:   prNumber,
			}

			err = session.CloseAnyOutstanding(ctx, tt.message)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("CloseAnyOutstanding: got error = %v, wanted nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("CloseAnyOutstanding: got error = %v, want error containing %q", err, tt.wantErr)
			}
			if tt.cancelAfterClose && ctx.Err() == nil {
				t.Fatal("reconcile context was not canceled, so the case proves nothing")
			}

			var wantComments []string
			if tt.wantComment != "" {
				wantComments = []string{tt.wantComment}
			}
			if !slicesEqual(comments, wantComments) {
				t.Errorf("comments: got = %q, want = %q", comments, wantComments)
			}
			if closed != tt.wantClosed {
				t.Errorf("PR closed: got = %t, want = %t", closed, tt.wantClosed)
			}
			if !slicesEqual(deleted, tt.wantDeleted) {
				t.Errorf("deleted refs: got = %q, want = %q", deleted, tt.wantDeleted)
			}
		})
	}
}

// TestRefAlreadyGone verifies which DeleteRef failures, as go-github reports
// them, count as the ref already being gone.
func TestRefAlreadyGone(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{{
		name:   "422 reference does not exist",
		status: http.StatusUnprocessableEntity,
		body:   `{"message":"Reference does not exist"}`,
		want:   true,
	}, {
		name:   "404",
		status: http.StatusNotFound,
		body:   `{"message":"Not Found"}`,
		want:   true,
	}, {
		name:   "422 with another message",
		status: http.StatusUnprocessableEntity,
		body:   `{"message":"Validation Failed"}`,
	}, {
		name:   "bare 422",
		status: http.StatusUnprocessableEntity,
	}, {
		name:   "403",
		status: http.StatusForbidden,
		body:   `{"message":"Resource not accessible by integration"}`,
	}, {
		name:   "500",
		status: http.StatusInternalServerError,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}
			_, err = client.Git.DeleteRef(t.Context(), "test-owner", "test-repo", "heads/test-bot/foo")
			if err == nil {
				t.Fatal("DeleteRef: got nil error, wanted one")
			}
			if got := refAlreadyGone(err); got != tt.want {
				t.Errorf("refAlreadyGone(%v): got = %t, want = %t", err, got, tt.want)
			}
		})
	}

	if refAlreadyGone(context.Canceled) {
		t.Error("refAlreadyGone(context.Canceled): got = true, want = false")
	}
}

// cancelAfterClose cancels the reconcile's context as soon as the close
// returns, standing in for a rollout's SIGTERM or a reconcile deadline. It
// buffers the close response first so the client can still decode it.
type cancelAfterClose struct {
	next   http.RoundTripper
	cancel context.CancelFunc
}

func (c *cancelAfterClose) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if err != nil || r.Method != http.MethodPatch {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	c.cancel()
	return resp, nil
}

// closedPR builds the PATCH response for a closed PR whose head is ref in the
// repository fullName.
func closedPR(ref, fullName string, labels ...string) *github.PullRequest {
	pr := &github.PullRequest{
		Number: new(99),
		State:  new("closed"),
		Head: &github.PullRequestBranch{
			Ref:  new(ref),
			Repo: &github.Repository{FullName: new(fullName)},
		},
	}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &github.Label{Name: new(l)})
	}
	return pr
}
