/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/commentack"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
)

func TestIsDiffTooLarge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "plain error",
			err:  errors.New("get PR diff: connection reset"),
			want: false,
		},
		{
			name: "406 without error entries",
			err:  &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotAcceptable}},
			want: true,
		},
		{
			name: "406 with too_large entry",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusNotAcceptable},
				Errors:   []github.Error{{Resource: "PullRequest", Field: "diff", Code: "too_large"}},
			},
			want: true,
		},
		{
			name: "too_large entry under another status",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
				Errors:   []github.Error{{Code: "too_large"}},
			},
			want: true,
		},
		{
			name: "404 is not too large",
			err:  &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}},
			want: false,
		},
		{
			name: "422 with an unrelated code",
			err: &github.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
				Errors:   []github.Error{{Code: "custom"}},
			},
			want: false,
		},
		{
			name: "wrapped 406",
			err:  fmt.Errorf("get PR diff: %w", &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotAcceptable}}),
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDiffTooLarge(tc.err); got != tc.want {
				t.Errorf("isDiffTooLarge: got = %v, want = %v", got, tc.want)
			}
		})
	}
}

func TestOwnsPR(t *testing.T) {
	tests := []struct {
		name              string
		viewerDidAuthor   bool
		isCrossRepository bool
		want              bool
	}{
		{
			name:            "app authored, same repo",
			viewerDidAuthor: true,
			want:            true,
		},
		{
			name:              "app authored but from a fork",
			viewerDidAuthor:   true,
			isCrossRepository: true,
			want:              false,
		},
		{
			name:            "someone else authored on a same-repo branch borrowing the prefix",
			viewerDidAuthor: false,
			want:            false,
		},
		{
			name:              "someone else authored from a fork",
			viewerDidAuthor:   false,
			isCrossRepository: true,
			want:              false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsPR(tc.viewerDidAuthor, tc.isCrossRepository); got != tc.want {
				t.Errorf("ownsPR(%v, %v): got = %v, want = %v", tc.viewerDidAuthor, tc.isCrossRepository, got, tc.want)
			}
		})
	}
}

func TestTookOver(t *testing.T) {
	const identity = "testbot"
	tests := []struct {
		name      string
		labels    []string
		assignees []string
		want      bool
	}{{
		name:   "skip label only",
		labels: []string{"skip:" + identity},
		want:   true,
	}, {
		name:      "assignee only",
		assignees: []string{"example-user"},
		want:      true,
	}, {
		name:      "skip label and assignee",
		labels:    []string{"skip:" + identity},
		assignees: []string{"example-user"},
		want:      true,
	}, {
		name:   "neither",
		labels: []string{"enhancement"},
		want:   false,
	}, {
		name:   "skip label for another identity",
		labels: []string{"skip:other-bot", "skip:" + identity + "-extra"},
		want:   false,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr := newPRWithLabels(tc.labels)
			for _, a := range tc.assignees {
				pr.Assignees = append(pr.Assignees, &github.User{Login: new(a)})
			}
			if got := tookOver(pr, identity); got != tc.want {
				t.Errorf("tookOver: got = %v, want = %v", got, tc.want)
			}
		})
	}
}

type fakeCommentAPI struct {
	comments      []commentack.Comment
	headCommitted time.Time
	listErr       error
	reacted       []string
}

func (f *fakeCommentAPI) List(context.Context, string, string, int) ([]commentack.Comment, time.Time, error) {
	return f.comments, f.headCommitted, f.listErr
}

func (f *fakeCommentAPI) AddEyes(_ context.Context, id string) error {
	f.reacted = append(f.reacted, id)
	return nil
}

// testComments returns an old, a recent, and an untrusted comment, relative to
// now. With a head commit older than an hour, only the recent one should get a
// 👀.
func testComments(now time.Time) []commentack.Comment {
	return []commentack.Comment{
		{NodeID: "old", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-48 * time.Hour)},
		{NodeID: "recent", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-time.Hour)},
		{NodeID: "stranger", AuthorType: "User", AuthorAssociation: "NONE", CreatedAt: now.Add(-time.Hour)},
	}
}

func TestAckOwnedPRComments(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name          string
		headCommitted time.Time
		listErr       error
		wantReacted   []string
	}{{
		name:          "acks recent trusted comments only",
		headCommitted: now.Add(-2 * time.Hour),
		wantReacted:   []string{"recent"},
	}, {
		// The comment is inside the 24-hour window but was left before the head
		// commit, so the bot will not act on it and must not promise to.
		name:          "a comment older than the head commit gets no eyes",
		headCommitted: now.Add(-30 * time.Minute),
	}, {
		name:          "a failed read is logged and acks nothing",
		headCommitted: now.Add(-2 * time.Hour),
		listErr:       errors.New("secondary rate limit"),
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeCommentAPI{comments: testComments(now), headCommitted: tc.headCommitted, listErr: tc.listErr}
			r := &core{ackComments: true, commentAPI: func(*github.Client) commentack.API { return api }}
			res := &githubreconciler.Resource{Owner: "o", Repo: "r", Number: 7}

			r.ackOwnedPRComments(t.Context(), nil, res, "example-app")

			if diff := cmp.Diff(tc.wantReacted, api.reacted); diff != "" {
				t.Errorf("reacted (-want, +got):\n%s", diff)
			}
		})
	}
}

// fakeGitHub answers the GitHub calls reconcilePullRequest makes for a pull
// request in o/r, and counts the ownership queries.
type fakeGitHub struct {
	pr               *github.PullRequest
	viewerDidAuthor  bool
	ownershipQueries int
	unexpected       []string
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/repos/o/r/pulls/7":
		return jsonResponse(req, f.pr)
	case req.Method == http.MethodPost && req.URL.Path == "/graphql":
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(body), "viewerDidAuthor") {
			break
		}
		f.ownershipQueries++
		return jsonResponse(req, map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequest": map[string]any{"viewerDidAuthor": f.viewerDidAuthor, "isCrossRepository": false},
		}}})
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
		return jsonResponse(req, map[string]any{"total_count": 0, "check_runs": []any{}})
	case req.Method == http.MethodPost && req.URL.Path == "/repos/o/r/check-runs":
		return jsonResponse(req, map[string]any{"id": 1})
	}
	f.unexpected = append(f.unexpected, req.Method+" "+req.URL.Path)
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)),
		Request:    req,
	}, nil
}

func jsonResponse(req *http.Request, v any) (*http.Response, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    req,
	}, nil
}

// TestReconcilePullRequestCommentAck drives reconcilePullRequest against a fake
// GitHub. It checks that the ack is off unless asked for, that it skips a pull
// request a person has taken over, comments that mention someone else and
// comments left before the head commit, and
// that ownership is asked about at most once per reconcile and only for a
// branch with the identity prefix.
func TestReconcilePullRequestCommentAck(t *testing.T) {
	// The status manager looks up the project ID; answer it locally so the
	// test never reaches for a real metadata server.
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		fmt.Fprint(w, "test-project")
	}))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))

	const identity = "testbot"
	sm, err := statusmanager.NewStatusManager[CheckDetails](t.Context(), identity)
	if err != nil {
		t.Fatalf("NewStatusManager: %v", err)
	}

	now := time.Now()
	tests := []struct {
		name            string
		enabled         bool
		branch          string
		labels          []string
		assignees       []string
		viewerDidAuthor bool
		// comments are what the fake comment API returns; nil means
		// testComments.
		comments []commentack.Comment
		// headAge is how long before now the head commit was made; zero means
		// two hours, older than every recent test comment.
		headAge     time.Duration
		wantQueries int
		wantReacted []string
		wantRequeue bool
	}{{
		name:        "off: branch without the prefix asks nothing about ownership",
		branch:      "feature/thing",
		wantQueries: 0,
	}, {
		name:            "off: skip label returns before any ownership query",
		branch:          identity + "/some-path",
		labels:          []string{"skip:" + identity},
		viewerDidAuthor: true,
		wantQueries:     0,
	}, {
		name:            "off: owned pull request asks about ownership once",
		branch:          identity + "/some-path",
		viewerDidAuthor: true,
		wantQueries:     1,
		wantRequeue:     true,
	}, {
		name:        "on: branch without the prefix asks nothing and acks nothing",
		enabled:     true,
		branch:      "feature/thing",
		wantQueries: 0,
	}, {
		// A person has taken over the pull request, so it gets no eyes, and Case 1
		// returns before any ownership query.
		name:            "on: owned pull request with the skip label gets no eyes",
		enabled:         true,
		branch:          identity + "/some-path",
		labels:          []string{"skip:" + identity},
		viewerDidAuthor: true,
		wantQueries:     0,
	}, {
		// An assignee also means a person has taken over, but without the skip
		// label Case 2 still runs: it asks about ownership once and requeues.
		name:            "on: owned pull request with an assignee gets no eyes",
		enabled:         true,
		branch:          identity + "/some-path",
		assignees:       []string{"example-user"},
		viewerDidAuthor: true,
		wantQueries:     1,
		wantRequeue:     true,
	}, {
		name:            "on: comment that mentions someone else gets no eyes",
		enabled:         true,
		branch:          identity + "/some-path",
		viewerDidAuthor: true,
		comments: []commentack.Comment{
			{NodeID: "for-someone-else", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-time.Hour), Body: "@example-user can you take a look?"},
			{NodeID: "for-the-bot", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-time.Hour), Body: "@example-app please rebase"},
		},
		wantQueries: 1,
		wantReacted: []string{"for-the-bot"},
		wantRequeue: true,
	}, {
		name:            "on: owned pull request asks about ownership once and acks",
		enabled:         true,
		branch:          identity + "/some-path",
		viewerDidAuthor: true,
		wantQueries:     1,
		wantReacted:     []string{"recent"},
		wantRequeue:     true,
	}, {
		// The recent comment is inside the 24-hour window but older than the
		// head commit, so the bot will not act on it and it gets no eyes.
		name:            "on: comment older than the head commit gets no eyes",
		enabled:         true,
		branch:          identity + "/some-path",
		viewerDidAuthor: true,
		headAge:         30 * time.Minute,
		wantQueries:     1,
		wantRequeue:     true,
	}, {
		name:            "on: branch that borrows the prefix acks nothing",
		enabled:         true,
		branch:          identity + "/some-path",
		viewerDidAuthor: false,
		wantQueries:     1,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr := newPRWithLabels(tc.labels)
			pr.State = new("open")
			pr.Head = &github.PullRequestBranch{Ref: new(tc.branch), SHA: new("0123456789abcdef")}
			pr.Base = &github.PullRequestBranch{Ref: new("main")}
			pr.User = &github.User{Login: new("example-app[bot]")}
			for _, a := range tc.assignees {
				pr.Assignees = append(pr.Assignees, &github.User{Login: new(a)})
			}
			fake := &fakeGitHub{pr: pr, viewerDidAuthor: tc.viewerDidAuthor}
			gh, err := github.NewClient(github.WithTransport(fake))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			comments := tc.comments
			if comments == nil {
				comments = testComments(now)
			}
			// The test file imports go-cmp as cmp, so the standard cmp.Or is not
			// at hand for this default.
			headAge := tc.headAge
			if headAge == 0 {
				headAge = 2 * time.Hour
			}
			api := &fakeCommentAPI{comments: comments, headCommitted: now.Add(-headAge)}
			r := &core{
				identity:      identity,
				statusManager: sm,
				ackComments:   tc.enabled,
				commentAPI:    func(*github.Client) commentack.API { return api },
			}
			res := &githubreconciler.Resource{Owner: "o", Repo: "r", Number: 7, Type: githubreconciler.ResourceTypePullRequest}

			err = r.reconcilePullRequest(t.Context(), res, gh)

			if gotRequeue := len(workqueue.GetQueueKeys(err)) > 0; gotRequeue != tc.wantRequeue {
				t.Errorf("requeue: got = %v, want = %v (err = %v)", gotRequeue, tc.wantRequeue, err)
			}
			if !tc.wantRequeue && err != nil {
				t.Errorf("reconcilePullRequest: got error %v, want nil", err)
			}
			if fake.ownershipQueries != tc.wantQueries {
				t.Errorf("ownership queries: got = %d, want = %d", fake.ownershipQueries, tc.wantQueries)
			}
			if diff := cmp.Diff(tc.wantReacted, api.reacted); diff != "" {
				t.Errorf("reacted (-want, +got):\n%s", diff)
			}
			if len(fake.unexpected) > 0 {
				t.Errorf("unexpected GitHub requests: %v", fake.unexpected)
			}
		})
	}
}
