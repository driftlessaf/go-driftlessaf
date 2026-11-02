/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v88/github"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestClient returns a GitHub client whose requests go to handler.
func newTestClient(t *testing.T, handler http.HandlerFunc) *github.Client {
	t.Helper()
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Result(), nil
	})}))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return gh
}

func TestClientList(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "pr_comments.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var query string
	gh := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		query = body.Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	})

	got, headCommitted, err := NewClient(gh).List(t.Context(), "example-owner", "example-repo", 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("comments: got = %d, want at least 2 (one conversation, one inline)", len(got))
	}
	for _, c := range got {
		if c.NodeID == "" || c.AuthorType == "" || c.AuthorAssociation == "" || c.CreatedAt.IsZero() {
			t.Errorf("comment is missing a field: %+v", c)
		}
		// Each fixture comment carries a 👀 from a person only, not a bot.
		if c.EyesFromBot {
			t.Errorf("EyesFromBot: got = true, want = false for %s", c.NodeID)
		}
	}
	// The fixture has no commits, so there is no cutoff.
	if !headCommitted.IsZero() {
		t.Errorf("head commit time: got = %v, want the zero time", headCommitted)
	}
	for _, want := range []string{"comments(last: 100)", "comments(last: 10)", "reviewThreads(first: 100)", "reactors(first: 10)", "body", "commits(last: 1)", "committedDate"} {
		if !strings.Contains(query, want) {
			t.Errorf("query does not contain %q:\n%s", want, query)
		}
	}
}

func TestClientListReactorsAndDeletedAuthor(t *testing.T) {
	gh := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"comments":{"nodes":[
			{"id":"bot-eyes","createdAt":"2026-10-27T14:16:24Z","isMinimized":false,"authorAssociation":"OWNER","body":"@example-app please rebase","author":{"__typename":"User"},
			 "reactionGroups":[{"content":"EYES","reactors":{"nodes":[{"__typename":"User"},{"__typename":"Bot"}]}}]},
			{"id":"bot-thumbs","createdAt":"2026-10-27T14:16:25Z","isMinimized":false,"authorAssociation":"OWNER","author":{"__typename":"User"},
			 "reactionGroups":[{"content":"THUMBS_UP","reactors":{"nodes":[{"__typename":"Bot"}]}},{"content":"EYES","reactors":{"nodes":[]}}]},
			{"id":"ghost","createdAt":"2026-10-27T14:16:26Z","isMinimized":false,"authorAssociation":"NONE","author":null,"reactionGroups":[]}
		]},"reviewThreads":{"nodes":[]}}}}}`)
	})

	got, _, err := NewClient(gh).List(t.Context(), "o", "r", 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	type summary struct {
		NodeID      string
		AuthorType  string
		Body        string
		EyesFromBot bool
	}
	gotSummary := make([]summary, 0, len(got))
	for _, c := range got {
		gotSummary = append(gotSummary, summary{NodeID: c.NodeID, AuthorType: c.AuthorType, Body: c.Body, EyesFromBot: c.EyesFromBot})
	}
	want := []summary{
		{NodeID: "bot-eyes", AuthorType: "User", Body: "@example-app please rebase", EyesFromBot: true},
		// A bot's 👍 is not a 👀.
		{NodeID: "bot-thumbs", AuthorType: "User", EyesFromBot: false},
		{NodeID: "ghost", AuthorType: "", EyesFromBot: false},
	}
	if diff := cmp.Diff(want, gotSummary); diff != "" {
		t.Errorf("comments (-want, +got):\n%s", diff)
	}
}

func TestClientListMissingPullRequest(t *testing.T) {
	gh := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
	})
	got, headCommitted, err := NewClient(gh).List(t.Context(), "o", "r", 7)
	if err != nil || got != nil || !headCommitted.IsZero() {
		t.Errorf("List: got = (%v, %v, %v), want = (nil, zero time, nil)", got, headCommitted, err)
	}
}

func TestClientListHeadCommit(t *testing.T) {
	const comment = `{"id":"IC_example_1","createdAt":"2026-10-27T14:16:24Z","isMinimized":false,"authorAssociation":"OWNER","body":"please rebase","author":{"__typename":"User"},"reactionGroups":[]}`

	tests := []struct {
		name         string
		commits      string
		want         time.Time
		wantComments bool
	}{{
		name:         "head commit date is carried through",
		commits:      `{"nodes":[{"commit":{"committedDate":"2026-10-27T14:00:00Z"}}]}`,
		want:         time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC),
		wantComments: true,
	}, {
		// The zero time means no cutoff, so falling back to it would put a 👀
		// on every comment in the window.
		name:    "a commit with an unreadable date gives no comments",
		commits: `{"nodes":[{"commit":{"committedDate":"not a date"}}]}`,
	}, {
		name:    "a commit with an empty date gives no comments",
		commits: `{"nodes":[{"commit":{"committedDate":""}}]}`,
	}, {
		name:         "no commit gives comments and the zero time",
		commits:      `{"nodes":[]}`,
		wantComments: true,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gh := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"comments":{"nodes":[`+comment+`]},"reviewThreads":{"nodes":[]},"commits":`+tc.commits+`}}}}`)
			})

			got, headCommitted, err := NewClient(gh).List(t.Context(), "example-owner", "example-repo", 7)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			switch {
			case !tc.wantComments && len(got) != 0:
				t.Errorf("comments: got = %+v, want none", got)
			case tc.wantComments && (len(got) != 1 || got[0].NodeID != "IC_example_1"):
				t.Errorf("comments: got = %+v, want IC_example_1", got)
			}
			if !headCommitted.Equal(tc.want) {
				t.Errorf("head commit time: got = %v, want = %v", headCommitted, tc.want)
			}
		})
	}
}

func TestClientAddEyes(t *testing.T) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	gh := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"addReaction":{"clientMutationId":null}}}`)
	})

	if err := NewClient(gh).AddEyes(t.Context(), "IC_node"); err != nil {
		t.Fatalf("AddEyes: %v", err)
	}
	if !strings.Contains(body.Query, "addReaction") {
		t.Errorf("query: got = %q, want an addReaction mutation", body.Query)
	}
	input, _ := body.Variables["input"].(map[string]any)
	if input["subjectId"] != "IC_node" || input["content"] != "EYES" {
		t.Errorf("input: got = %v, want subjectId IC_node and content EYES", input)
	}
}

// fakeAPI records the reactions Ack asks for.
type fakeAPI struct {
	comments      []Comment
	headCommitted time.Time
	listErr       error
	failOn        string
	reacted       []string
}

func (f *fakeAPI) List(context.Context, string, string, int) ([]Comment, time.Time, error) {
	return f.comments, f.headCommitted, f.listErr
}

func (f *fakeAPI) AddEyes(_ context.Context, id string) error {
	if id == f.failOn {
		return errors.New("forbidden")
	}
	f.reacted = append(f.reacted, id)
	return nil
}

func TestAck(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	comment := func(id string, age time.Duration) Comment {
		return Comment{NodeID: id, AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-age)}
	}

	tests := []struct {
		name string
		api  *fakeAPI
		// callerSince is the Since the caller passes in, which Ack replaces.
		callerSince time.Time
		wantReacted []string
		wantErr     string
	}{{
		name:        "reacts oldest first",
		api:         &fakeAPI{comments: []Comment{comment("b", time.Minute), comment("a", time.Hour)}},
		wantReacted: []string{"a", "b"},
	}, {
		name:        "one failure does not stop the others",
		api:         &fakeAPI{comments: []Comment{comment("a", time.Hour), comment("b", time.Minute)}, failOn: "a"},
		wantReacted: []string{"b"},
		wantErr:     "forbidden",
	}, {
		name:    "list failure is returned",
		api:     &fakeAPI{listErr: errors.New("boom")},
		wantErr: "boom",
	}, {
		name: "nothing to do",
		api:  &fakeAPI{comments: []Comment{comment("old", 48*time.Hour)}},
	}, {
		// Both comments are inside the window, but only one came after the head
		// commit, so only that one is acted on and only that one gets a 👀.
		name: "a comment before the head commit gets no eyes",
		api: &fakeAPI{
			comments:      []Comment{comment("before", time.Hour), comment("after", time.Minute)},
			headCommitted: now.Add(-30 * time.Minute),
		},
		wantReacted: []string{"after"},
	}, {
		name: "an inline comment before the head commit still gets eyes",
		api: &fakeAPI{
			comments:      []Comment{{NodeID: "inline", AuthorType: "User", AuthorAssociation: "OWNER", CreatedAt: now.Add(-time.Hour), Inline: true}, comment("conversation", time.Hour)},
			headCommitted: now.Add(-30 * time.Minute),
		},
		wantReacted: []string{"inline"},
	}, {
		name: "the head commit date replaces the caller's Since",
		api: &fakeAPI{
			comments:      []Comment{comment("a", time.Hour), comment("b", time.Minute)},
			headCommitted: now.Add(-2 * time.Hour),
		},
		callerSince: now,
		wantReacted: []string{"a", "b"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Ack(t.Context(), tc.api, "o", "r", 1, Options{Now: now, Window: 24 * time.Hour, Since: tc.callerSince, Trusted: func(string) bool { return true }})
			if (err != nil) != (tc.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("Ack error: got = %v, want containing %q", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.wantReacted, tc.api.reacted); diff != "" {
				t.Errorf("reacted (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestClientListInlineAndResolvedThreads(t *testing.T) {
	gh := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"comments":{"nodes":[
			{"id":"conversation","createdAt":"2026-10-27T14:16:24Z","isMinimized":false,"authorAssociation":"OWNER","author":{"__typename":"User"},"reactionGroups":[]}
		]},"reviewThreads":{"nodes":[
			{"isResolved":false,"comments":{"nodes":[
				{"id":"open","createdAt":"2026-10-27T14:16:25Z","isMinimized":false,"authorAssociation":"OWNER","author":{"__typename":"User"},"reactionGroups":[]}
			]}},
			{"isResolved":true,"comments":{"nodes":[
				{"id":"resolved","createdAt":"2026-10-27T14:16:26Z","isMinimized":false,"authorAssociation":"OWNER","author":{"__typename":"User"},"reactionGroups":[]}
			]}}
		]}}}}}`)
	})

	got, _, err := NewClient(gh).List(t.Context(), "o", "r", 7)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	type summary struct {
		NodeID string
		Inline bool
	}
	gotSummary := make([]summary, 0, len(got))
	for _, c := range got {
		gotSummary = append(gotSummary, summary{NodeID: c.NodeID, Inline: c.Inline})
	}
	// The comment in the resolved thread is not listed.
	want := []summary{{NodeID: "conversation"}, {NodeID: "open", Inline: true}}
	if diff := cmp.Diff(want, gotSummary); diff != "" {
		t.Errorf("comments (-want, +got):\n%s", diff)
	}
}
