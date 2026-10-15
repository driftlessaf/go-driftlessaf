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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// viewerLogin is the login the fake GitHub attributes the bot's own comments
// to; the IssueThread query reports viewerDidAuthor for them.
const viewerLogin = "test-bot[bot]"

// requiredLabel is the label the harness's reconciler requires.
const requiredLabel = "test-bot/managed"

// timelineEvent is a label or reopen event on the harness's issue.
type timelineEvent struct {
	typename string // LabeledEvent or ReopenedEvent
	label    string
	at       time.Time
}

// issueHarness drives reconcileIssue against a fake GitHub and a local remote.
// The issue's comment thread and timeline are stateful: comments the bot posts
// and deletes change them, so successive reconciles see what earlier ones
// left, the way the bot's own comment events would deliver them.
type issueHarness struct {
	mu sync.Mutex

	issue    *github.Issue
	comments []*github.IssueComment
	events   []timelineEvent
	nextID   int64
	now      time.Time

	// createFails and deleteFails make the bot's comment posts and deletes fail.
	createFails bool
	deleteFails bool

	agentCalls int
	prCreates  int
}

func newIssueHarness(t *testing.T) *issueHarness {
	t.Helper()
	// SetRepoURLForTesting is process-global, so callers must not run in parallel.
	t.Setenv("TMPDIR", t.TempDir())
	remoteDir, _ := publishRepository(t)
	t.Cleanup(clonemanager.SetRepoURLForTesting(func(*githubreconciler.Resource) string { return remoteDir }))
	h := &issueHarness{
		issue: &github.Issue{
			Number:  new(123),
			Title:   new("Do the thing"),
			Body:    new("Please do the thing."),
			HTMLURL: new("https://github.com/owner/repo/issues/123"),
			State:   new("open"),
			Labels:  []*github.Label{{Name: new(requiredLabel)}},
		},
		nextID: 1,
		now:    time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
	}
	h.addEvent("LabeledEvent", requiredLabel)
	return h
}

// addComment appends a comment by login to the issue's thread.
func (h *issueHarness) addComment(login, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendComment(login, body)
}

// appendComment is addComment for a caller holding h.mu.
func (h *issueHarness) appendComment(login, body string) *github.IssueComment {
	c := &github.IssueComment{ID: new(h.nextID), NodeID: new(fmt.Sprintf("IC_%d", h.nextID)), Body: new(body), User: &github.User{Login: new(login)}}
	h.comments = append(h.comments, c)
	h.nextID++
	return c
}

// addEvent appends a timeline event a minute after the previous one.
func (h *issueHarness) addEvent(typename, label string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.events = append(h.events, timelineEvent{typename: typename, label: label, at: h.now})
}

// setLabels replaces the issue's labels.
func (h *issueHarness) setLabels(names ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.issue.Labels = nil
	for _, n := range names {
		h.issue.Labels = append(h.issue.Labels, &github.Label{Name: new(n)})
	}
}

// setState sets the issue's state.
func (h *issueHarness) setState(state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.issue.State = new(state)
}

// notes returns the bodies of the bot's own comments beginning with the
// give-up marker, oldest first.
func (h *issueHarness) notes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var notes []string
	for _, c := range h.comments {
		if c.GetUser().GetLogin() == viewerLogin && strings.HasPrefix(c.GetBody(), giveUpMarker) {
			notes = append(notes, c.GetBody())
		}
	}
	return notes
}

// latest returns the thread's last comment, or nil when it is empty.
func (h *issueHarness) latest() *github.IssueComment {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.comments) == 0 {
		return nil
	}
	return h.comments[len(h.comments)-1]
}

// countPrefixed reports how many comments begin with prefix.
func (h *issueHarness) countPrefixed(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.comments {
		if strings.HasPrefix(c.GetBody(), prefix) {
			n++
		}
	}
	return n
}

// commentPageSize matches the page size of the IssueThread query; the fake
// serves pages of whatever size the query asks for, and refuses a query whose
// page size it cannot read.
var commentPageSize = regexp.MustCompile(`comments\(first: (\d+), after: \$after\)`)

// graphQLRequest is the body githubv4 posts.
type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// serveGraphQL answers the GraphQL requests a no-PR reconcile makes: the
// session's PR lookup, the IssueThread query, and deleteIssueComment. h.mu is
// held.
func (h *issueHarness) serveGraphQL(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var req graphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode GraphQL request: %v", err)
	}
	reply := func(data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Errorf("encode GraphQL reply: %v", err)
		}
	}
	switch {
	case strings.Contains(req.Query, "deleteIssueComment("):
		if h.deleteFails {
			fmt.Fprint(w, `{"errors":[{"message":"delete failed"}]}`)
			return
		}
		input, _ := req.Variables["input"].(map[string]any)
		id, _ := input["id"].(string)
		i := slices.IndexFunc(h.comments, func(c *github.IssueComment) bool { return c.GetNodeID() == id })
		if i < 0 {
			fmt.Fprintf(w, `{"errors":[{"type":"NOT_FOUND","message":"no comment %s"}]}`, id)
			return
		}
		h.comments = slices.Delete(h.comments, i, i+1)
		reply(map[string]any{"deleteIssueComment": map[string]any{"clientMutationId": nil}})

	case strings.Contains(req.Query, "timelineItems("):
		m := commentPageSize.FindStringSubmatch(req.Query)
		if m == nil {
			t.Errorf("IssueThread query: got = %q, want comments(first: N, after: $after)", req.Query)
			return
		}
		size, _ := strconv.Atoi(m[1])
		start := 0
		if after, ok := req.Variables["after"].(string); ok {
			start, _ = strconv.Atoi(after)
		}
		end := min(start+size, len(h.comments))
		nodes := []map[string]any{}
		for _, c := range h.comments[start:end] {
			typename := "User"
			if strings.HasSuffix(c.GetUser().GetLogin(), "[bot]") {
				typename = "Bot"
			}
			nodes = append(nodes, map[string]any{
				"id":              c.GetNodeID(),
				"body":            c.GetBody(),
				"viewerDidAuthor": c.GetUser().GetLogin() == viewerLogin,
				"author":          map[string]any{"__typename": typename},
			})
		}
		events := h.events
		if n, ok := req.Variables["events"].(float64); ok && int(n) < len(events) {
			events = events[len(events)-int(n):]
		}
		items := []map[string]any{}
		for _, e := range events {
			item := map[string]any{"__typename": e.typename, "createdAt": e.at.Format(time.RFC3339)}
			if e.typename == "LabeledEvent" {
				item["label"] = map[string]any{"name": e.label}
			}
			items = append(items, item)
		}
		reply(map[string]any{"repository": map[string]any{"issue": map[string]any{
			"comments": map[string]any{
				"nodes":    nodes,
				"pageInfo": map[string]any{"hasNextPage": end < len(h.comments), "endCursor": strconv.Itoa(end)},
			},
			"timelineItems": map[string]any{"nodes": items},
		}}})

	default:
		reply(map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}})
	}
}

func (h *issueHarness) client(t *testing.T) *github.Client {
	t.Helper()
	gh, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: publishTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			h.serveGraphQL(t, w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/123":
			h.issue.Comments = new(len(h.comments))
			if err := json.NewEncoder(w).Encode(h.issue); err != nil {
				t.Errorf("encode issue: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/123/comments":
			if err := json.NewEncoder(w).Encode(h.comments); err != nil {
				t.Errorf("encode comments: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/123/comments":
			if h.createFails {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"create failed"}`)
				return
			}
			var c github.IssueComment
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			if err := json.NewEncoder(w).Encode(h.appendComment(viewerLogin, c.GetBody())); err != nil {
				t.Errorf("encode comment: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/compare/main...test-bot/issue-123":
			fmt.Fprint(w, `{"files":[{"filename":"content.txt"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			h.prCreates++
			fmt.Fprint(w, `{"number":456,"html_url":"https://github.com/owner/repo/pull/456"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/456/comments":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/456/labels":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("GitHub request: got = %s %s, want = supported request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})}}))
	if err != nil {
		t.Fatalf("create GitHub client: %v", err)
	}
	return gh
}

// reconcile builds a reconciler around agent and runs one reconcile of the issue.
func (h *issueHarness) reconcile(t *testing.T, agent func(context.Context) error, opts ...Option[*testRequest, *publishResult, struct{}]) {
	t.Helper()
	cm, err := changemanager.New[PRData[*testRequest]]("test-bot",
		template.Must(template.New("title").Parse("Update content")),
		template.Must(template.New("body").Parse("Resolve #123.")))
	if err != nil {
		t.Fatalf("create change manager: %v", err)
	}
	rec := New("test-bot", cm,
		clonemanager.NewMeta(t.Context(), func(context.Context, string, string) (oauth2.TokenSource, error) {
			return clonemanager.StaticTokenSource(""), nil
		}, "test-bot", nil),
		[]string{requiredLabel},
		&publishAgent{execute: func(ctx context.Context) error {
			h.mu.Lock()
			h.agentCalls++
			h.mu.Unlock()
			return agent(ctx)
		}},
		func(context.Context, *github.Issue, *changemanager.Session[PRData[*testRequest]]) (*testRequest, error) {
			return &testRequest{}, nil
		},
		func(context.Context, *changemanager.Session[PRData[*testRequest]], *clonemanager.Lease) (struct{}, error) {
			return struct{}{}, nil
		},
		append([]Option[*testRequest, *publishResult, struct{}]{
			WithRequiredLabel[*testRequest, *publishResult, struct{}](requiredLabel),
		}, opts...)...,
	)
	if err := rec.reconcileIssue(t.Context(), &githubreconciler.Resource{Owner: "owner", Repo: "repo", Number: 123, Type: githubreconciler.ResourceTypeIssue}, h.client(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// step runs one reconcile with a give-up comment and checks the running total
// of agent calls.
func (h *issueHarness) step(t *testing.T, name string, agent func(context.Context) error, wantCalls int, opts ...Option[*testRequest, *publishResult, struct{}]) {
	t.Helper()
	h.reconcile(t, agent, append([]Option[*testRequest, *publishResult, struct{}]{withGiveUp()}, opts...)...)
	if h.agentCalls != wantCalls {
		t.Errorf("%s: agent calls: got = %d, want = %d", name, h.agentCalls, wantCalls)
	}
}

const giveUpMarker = "<!--test:no-changes-->"

// withGiveUp configures the give-up comment the guard keys on.
func withGiveUp() Option[*testRequest, *publishResult, struct{}] {
	return WithGiveUpComment[*testRequest, *publishResult, struct{}](giveUpMarker, func(e string) string { return e })
}

// noChange is an agent that leaves the worktree clean, so the run gives up.
func noChange(context.Context) error { return nil }

// TestReconcileIssueGiveUpGuard drives the events that can follow a give-up
// note. The bot's own comments come back as issue events, so an event that
// asks for nothing new must not run the agent again, or the bot would repeat
// the same work on every event; and each way a person asks for another
// attempt must start one, or the bot stops listening to the issue. A run
// that gives up again must settle on its own next event with exactly one
// note, posted after whatever prompted it so the asker is notified.
func TestReconcileIssueGiveUpGuard(t *testing.T) {
	tests := []struct {
		name    string
		after   func(t *testing.T, h *issueHarness)
		wantRun bool
	}{{
		name:  "the note's own event",
		after: func(*testing.T, *issueHarness) {},
	}, {
		name:    "a person replies",
		after:   func(_ *testing.T, h *issueHarness) { h.addComment("filer", "it is still broken, please try again") },
		wantRun: true,
	}, {
		name:    "a person pastes the bot's note",
		after:   func(_ *testing.T, h *issueHarness) { h.addComment("filer", h.notes()[0]) },
		wantRun: true,
	}, {
		// Two bots that each counted the other's comments as a reply would
		// run each other in a loop.
		name:  "another bot comments",
		after: func(_ *testing.T, h *issueHarness) { h.addComment("other-bot[bot]", "noted") },
	}, {
		name: "another bot posts a note under the same marker",
		after: func(_ *testing.T, h *issueHarness) {
			h.addComment("other-bot[bot]", giveUpMarker+"\nnothing to do here either")
		},
	}, {
		name: "the issue body is edited",
		after: func(_ *testing.T, h *issueHarness) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.issue.Body = new("Please do the thing, and the other thing.")
		},
		wantRun: true,
	}, {
		// The queue can fold the unlabel and relabel events into one
		// reconcile that sees the label present and the note still there.
		name: "the required label is toggled in one coalesced event",
		after: func(_ *testing.T, h *issueHarness) {
			h.addEvent("LabeledEvent", requiredLabel)
		},
		wantRun: true,
	}, {
		name:  "an unrelated label is added",
		after: func(_ *testing.T, h *issueHarness) { h.addEvent("LabeledEvent", "triage") },
	}, {
		// The closed path keeps the note as the record of why no PR
		// appeared, so the note is still there when the issue reopens.
		name: "the issue is closed and reopened",
		after: func(t *testing.T, h *issueHarness) {
			h.setState("closed")
			h.step(t, "closing", noChange, 1)
			h.setState("open")
			h.addEvent("ReopenedEvent", "")
		},
		wantRun: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIssueHarness(t)
			h.step(t, "first run gives up", noChange, 1)
			tt.after(t, h)
			want := 1
			if tt.wantRun {
				want = 2
			}
			h.step(t, "the event", noChange, want)
			h.step(t, "the next event", noChange, want)
			if n := len(h.notes()); n != 1 {
				t.Errorf("bot's notes: got = %d, want = 1", n)
			}
			if got := h.latest(); tt.wantRun && got.GetUser().GetLogin() != viewerLogin {
				t.Errorf("latest comment: got = %q by %q, want the bot's note after the request", got.GetBody(), got.GetUser().GetLogin())
			}
		})
	}
}

// TestReconcileIssueReplyDuringRun proves a reply posted while the agent runs
// gets a run of its own. The agent never saw the reply, which may say that
// something blocking the work has since landed; if the note posted at the end
// of the run counted the reply as answered, the request would be dropped.
func TestReconcileIssueReplyDuringRun(t *testing.T) {
	h := newIssueHarness(t)
	h.step(t, "first run, during which a person replies", func(context.Context) error {
		h.addComment("filer", "the dependency landed, please retry")
		return nil
	}, 1)
	h.step(t, "the reply's event", noChange, 2)
	h.step(t, "the next note's event", noChange, 2)
}

// TestReconcileIssueNoteBehindLongThread proves a note posted after more than
// one page of comments still guards: a guard that read only the first page
// would miss it and run the agent on every event of a busy issue.
func TestReconcileIssueNoteBehindLongThread(t *testing.T) {
	h := newIssueHarness(t)
	for i := range 150 {
		h.addComment(fmt.Sprintf("person-%d", i), "+1")
	}
	h.step(t, "first run gives up", noChange, 1)
	h.step(t, "the note's own event", noChange, 1)
}

// TestReconcileIssueGiveUpWriteFailures proves a failed post or delete never
// leaves the issue without a note and never lets duplicates pile up. A
// missing note would leave the filer with no explanation and nothing to
// guard the next event; duplicates from a failed delete must still guard and
// be removed by the next give-up.
func TestReconcileIssueGiveUpWriteFailures(t *testing.T) {
	h := newIssueHarness(t)
	h.step(t, "first run gives up", noChange, 1)
	first := h.notes()

	h.createFails = true
	h.addComment("filer", "please try again")
	h.step(t, "a reply whose give-up cannot post", noChange, 2)
	if got := h.notes(); !slices.Equal(got, first) {
		t.Errorf("notes after a failed post: got = %q, want = %q", got, first)
	}

	h.createFails, h.deleteFails = false, true
	h.step(t, "a retry whose old note cannot be deleted", noChange, 3)
	if n := len(h.notes()); n != 2 {
		t.Errorf("notes after a failed delete: got = %d, want = 2", n)
	}
	h.step(t, "the duplicate's event", noChange, 3)

	h.deleteFails = false
	h.addComment("filer", "and again")
	h.step(t, "another reply", noChange, 4)
	if n := len(h.notes()); n != 1 {
		t.Errorf("notes after the next give-up: got = %d, want = 1", n)
	}
}

// TestReconcileIssueClearLeavesCopiedMarker proves the bot deletes only the
// notes it wrote. Its token may delete any comment on the issue, so matching
// the marker alone would let it erase a filer's comment that quotes a note.
func TestReconcileIssueClearLeavesCopiedMarker(t *testing.T) {
	h := newIssueHarness(t)
	copied := giveUpMarker + "\nquoted from an earlier run"
	h.addComment("filer", copied)
	h.step(t, "first run gives up", noChange, 1)
	if n := len(h.notes()); n != 1 {
		t.Errorf("bot's notes after the give-up: got = %d, want = 1", n)
	}

	h.setLabels()
	h.step(t, "the label is taken off", noChange, 1)
	if n := len(h.notes()); n != 0 {
		t.Errorf("bot's notes after the label is taken off: got = %d, want = 0", n)
	}
	if n := h.countPrefixed(copied); n != 1 {
		t.Errorf("filer's copied comments: got = %d, want = 1", n)
	}

	h.setLabels(requiredLabel)
	h.addEvent("LabeledEvent", requiredLabel)
	h.step(t, "the label is put back", noChange, 2)
}

// TestReconcileIssuePROpenClearsNotes proves the notes go once a later run
// opens a PR: the PR disproves "nothing to do", and a note left beside it
// would tell the filer otherwise.
func TestReconcileIssuePROpenClearsNotes(t *testing.T) {
	h := newIssueHarness(t)
	h.step(t, "first run gives up", noChange, 1)
	h.addComment("filer", "it is broken, look again")
	h.step(t, "the reply's run opens a PR", func(ctx context.Context) error {
		wt, ok := clonemanager.WorktreeFromContext(ctx)
		if !ok {
			return errors.New("missing leased worktree")
		}
		return os.WriteFile(filepath.Join(wt.Filesystem.Root(), "content.txt"), []byte("changed\n"), 0o644)
	}, 2)
	if h.prCreates != 1 {
		t.Errorf("PRs created: got = %d, want = 1", h.prCreates)
	}
	if n := len(h.notes()); n != 0 {
		t.Errorf("bot's notes after the PR opened: got = %d, want = 0", n)
	}
}

// TestReconcileIssueGuardWithStartComment proves the start comment, another
// of the bot's own issue comments, neither lifts the guard nor multiplies:
// were it counted as a reply, every run would ask for the next.
func TestReconcileIssueGuardWithStartComment(t *testing.T) {
	const startMarker = "<!--test:started-->"
	start := WithStartComment[*testRequest, *publishResult, struct{}](startMarker, func() string { return startMarker + "\nworking on it" })
	h := newIssueHarness(t)
	h.step(t, "first run gives up", noChange, 1, start)
	h.step(t, "the note's own event", noChange, 1, start)
	h.addComment("filer", "please try again")
	h.step(t, "the reply runs the agent, which gives up again", noChange, 2, start)
	h.step(t, "the fresh note's event", noChange, 2, start)
	if n := h.countPrefixed(startMarker); n != 1 {
		t.Errorf("start comments: got = %d, want = 1", n)
	}
}

// TestReconcileIssueWithoutGiveUpComment proves the guard reads nothing and
// holds nothing for a bot with no give-up comment configured: such a bot
// leaves no note to key on.
func TestReconcileIssueWithoutGiveUpComment(t *testing.T) {
	h := newIssueHarness(t)
	h.addComment(viewerLogin, giveUpMarker+"\nleft by an earlier configuration")
	h.reconcile(t, noChange)
	h.reconcile(t, noChange)
	if h.agentCalls != 2 {
		t.Errorf("agent calls: got = %d, want = 2", h.agentCalls)
	}
}
