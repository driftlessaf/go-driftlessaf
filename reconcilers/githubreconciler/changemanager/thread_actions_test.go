/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-github/v88/github"
)

// mutationRecorder is a GraphQL double that records, in order, which
// review-thread request each call carried and for which known thread.
// omitCommentID answers a reply with a payload that carries no comment, and
// resolved is the state reported for any thread whose state is read back.
type mutationRecorder struct {
	threads       []string
	calls         []string
	omitCommentID bool
	resolved      bool
}

// classifyMutation names the review-thread request a GraphQL body carries and
// the known thread it targets: a reply, a resolution, or a read-back of the
// thread's state name the thread, and a retraction names a comment whose id
// embeds it (see replyCommentID).
func classifyMutation(body string, threads []string) (kind, thread string) {
	kind, thread = "other", "?"
	switch {
	case strings.Contains(body, "addPullRequestReviewThreadReply"):
		kind = "reply"
	case strings.Contains(body, "resolveReviewThread"):
		kind = "resolve"
	case strings.Contains(body, "deletePullRequestReviewComment"):
		kind = "delete"
	case strings.Contains(body, "on PullRequestReviewThread"):
		kind = "state"
	}
	for _, id := range threads {
		if strings.Contains(body, id) {
			thread = id
		}
	}
	return kind, thread
}

// replyCommentID is the node id the double assigns to the reply posted on a
// thread. It embeds the thread id so a retraction of that comment classifies
// under its thread.
func replyCommentID(thread string) string { return "PRRC_" + thread }

// recordMutations returns the recorder's handler. A reply is answered with the
// comment id replyCommentID assigns to its thread, and a state read-back with
// the recorder's resolved state.
func recordMutations(m *mutationRecorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		kind, thread := classifyMutation(string(b), m.threads)
		m.calls = append(m.calls, kind+":"+thread)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case kind == "reply" && !m.omitCommentID:
			fmt.Fprintf(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":%q}}}}`, replyCommentID(thread))
		case kind == "state":
			fmt.Fprintf(w, `{"data":{"node":{"isResolved":%t}}}`, m.resolved)
		default:
			io.WriteString(w, `{"data":{}}`)
		}
	})
}

// failMutations wraps the recorder's handler so each mutation named in fail
// (as kind:thread) is recorded as kind-failed:thread and answered with a
// gateway error; every other request reaches the recorder.
func failMutations(m *mutationRecorder, fail ...string) http.Handler {
	inner := recordMutations(m)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		kind, thread := classifyMutation(body, m.threads)
		if slices.Contains(fail, kind+":"+thread) {
			m.calls = append(m.calls, kind+"-failed:"+thread)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(body))
		inner.ServeHTTP(w, r)
	})
}

func recordThreadActions(t *testing.T, cb callbacks.FindingCallbacks) {
	t.Helper()
	for _, step := range []func() error{
		func() error { return cb.Reply(t.Context(), "PRRT_a", "Fixed by bounding the read.") },
		func() error { return cb.Resolve(t.Context(), "PRRT_a") },
		func() error { return cb.Resolve(t.Context(), "PRRT_a") }, // asked twice, resolved once
		func() error { return cb.Reply(t.Context(), "PRRT_b", "Not a defect: the value is validated first.") },
	} {
		if err := step(); err != nil {
			t.Fatalf("recording thread action: %v", err)
		}
	}
}

// TestFlushThreadActions proves the queued replies and resolutions reach
// GitHub only at the flush, replies before resolutions, and that a run with no
// commit posts only the refutation (a reply on a thread the agent did not ask
// to resolve) and drops the rest.
func TestFlushThreadActions(t *testing.T) {
	tests := []struct {
		name      string
		committed bool
		wantCalls []string
	}{
		{
			name:      "after a push, replies go out first and then the resolutions",
			committed: true,
			wantCalls: []string{"reply:PRRT_a", "reply:PRRT_b", "resolve:PRRT_a"},
		},
		{
			name:      "without a commit, only the refutation reply goes out",
			committed: false,
			wantCalls: []string{"reply:PRRT_b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &mutationRecorder{threads: []string{"PRRT_a", "PRRT_b"}}
			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: true},
				gqlClient: newTestGraphQLClient(t, recordMutations(rec)),
				findings: []callbacks.Finding{
					{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
					{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
				},
			}
			recordThreadActions(t, s.FindingCallbacks())
			if len(rec.calls) != 0 {
				t.Fatalf("requests sent before the flush: %v", rec.calls)
			}
			s.flushThreadActions(t.Context(), tc.committed)
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("mutations: got = %v, want = %v", rec.calls, tc.wantCalls)
			}
			// The queue is consumed: a second flush has nothing to send.
			s.flushThreadActions(t.Context(), true)
			if len(rec.calls) != len(tc.wantCalls) {
				t.Errorf("second flush sent more requests: %v", rec.calls)
			}
		})
	}
}

func TestDropThreadActions(t *testing.T) {
	rec := &mutationRecorder{threads: []string{"PRRT_a", "PRRT_b"}}
	s := &Session[testData]{
		manager:   &CM[testData]{findingReplies: true},
		gqlClient: newTestGraphQLClient(t, recordMutations(rec)),
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
		},
	}
	recordThreadActions(t, s.FindingCallbacks())
	s.dropThreadActions(t.Context())
	s.flushThreadActions(t.Context(), true)
	if len(rec.calls) != 0 {
		t.Errorf("dropped actions were sent: %v", rec.calls)
	}
}

// TestUpsertAppliesThreadActionsByOutcome drives Upsert with a makeChanges
// that queues a fix reply and a resolution, and checks the mutations are sent
// only after makeChanges returns and only when it pushed a change.
func TestUpsertAppliesThreadActionsByOutcome(t *testing.T) {
	tests := []struct {
		name      string
		makeErr   error
		wantCalls []string
		wantErrIs error
	}{
		{name: "pushed change applies the reply and the resolution", wantCalls: []string{"reply:PRRT_a", "resolve:PRRT_a"}},
		{name: "no change drops a fix claim and its resolution", makeErr: ErrNoChanges, wantErrIs: ErrNoChanges},
		{name: "failed run drops everything", makeErr: errors.New("verification failed"), wantErrIs: errors.New("verification failed")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v3/repos/test-owner/test-repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				writeJSON(t, w, &github.PullRequest{Number: new(7), HTMLURL: new("https://example.test/pull/7")})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client, err := github.NewClient(github.WithEnterpriseURLs(srv.URL, srv.URL))
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}
			cm, err := New[testData]("test-bot",
				template.Must(template.New("title").Parse("{{.PackageName}}")),
				template.Must(template.New("body").Parse("Update {{.PackageName}}")),
				WithCloseOnEmptyDiff[testData](false), WithFindingReplies[testData]())
			if err != nil {
				t.Fatalf("creating CM: %v", err)
			}
			rec := &mutationRecorder{threads: []string{"PRRT_a"}}
			session := &Session[testData]{
				manager:    cm,
				client:     client,
				gqlClient:  newTestGraphQLClient(t, recordMutations(rec)),
				owner:      "test-owner",
				repo:       "test-repo",
				branchName: "test-bot/pkg",
				ref:        "main",
				findings:   []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"}},
			}

			var sentDuringChanges int
			_, err = session.Upsert(t.Context(), &testData{PackageName: "pkg"}, false, nil, func(ctx context.Context, _ string) error {
				cb := session.FindingCallbacks()
				if err := cb.Reply(ctx, "PRRT_a", "Fixed by bounding the read."); err != nil {
					return err
				}
				if err := cb.Resolve(ctx, "PRRT_a"); err != nil {
					return err
				}
				sentDuringChanges = len(rec.calls)
				return tc.makeErr
			})
			if sentDuringChanges != 0 {
				t.Errorf("%d mutation(s) sent while changes were still being made", sentDuringChanges)
			}
			switch {
			case tc.wantErrIs == nil && err != nil:
				t.Fatalf("Upsert: %v", err)
			case tc.wantErrIs != nil && err == nil:
				t.Fatalf("Upsert succeeded, want an error wrapping %v", tc.wantErrIs)
			case tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) && !strings.Contains(err.Error(), tc.wantErrIs.Error()):
				t.Fatalf("Upsert error: got = %v, want %v", err, tc.wantErrIs)
			}
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("mutations: got = %v, want = %v", rec.calls, tc.wantCalls)
			}
		})
	}
}

// TestFlushThreadActionsLeavesThreadOpenWhenReplyFails proves a resolution is
// applied only once its disposition reply posted: a thread whose reply failed
// stays open, while an unrelated thread is still resolved.
func TestFlushThreadActionsLeavesThreadOpenWhenReplyFails(t *testing.T) {
	rec := &mutationRecorder{threads: []string{"PRRT_a", "PRRT_b"}}
	s := &Session[testData]{
		manager:   &CM[testData]{findingReplies: true},
		gqlClient: newTestGraphQLClient(t, failMutations(rec, "reply:PRRT_a")),
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
		},
	}
	recordFixedThreads(t, s.FindingCallbacks(), "PRRT_a", "PRRT_b")
	s.flushThreadActions(t.Context(), true)
	want := []string{"reply-failed:PRRT_a", "reply:PRRT_b", "resolve:PRRT_b"}
	if !slices.Equal(rec.calls, want) {
		t.Errorf("mutations: got = %v, want = %v (no resolve for the thread whose reply failed)", rec.calls, want)
	}
}

// recordFixedThreads queues a fix reply and a resolution on each thread.
func recordFixedThreads(t *testing.T, cb callbacks.FindingCallbacks, threads ...string) {
	t.Helper()
	for _, id := range threads {
		if err := cb.Reply(t.Context(), id, "Fixed by checking the error."); err != nil {
			t.Fatalf("Reply(%s): %v", id, err)
		}
		if err := cb.Resolve(t.Context(), id); err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
	}
}

// TestResolveRequiresQueuedReply proves that, with replies enabled, a
// resolution is refused at the tool call, with an error the agent can act on,
// until a reply is queued on the thread, so an agent that skips the reply is
// told to post one rather than having its resolution dropped at the flush. A
// consumer without replies has no disposition to post, so its resolution
// stands on the pushed commit alone.
func TestResolveRequiresQueuedReply(t *testing.T) {
	tests := []struct {
		name       string
		replies    bool
		replyFirst bool
		wantErr    string
	}{
		{name: "replies enabled: resolving without a reply is refused", replies: true, wantErr: "reply to the finding before resolving it"},
		{name: "replies enabled: resolving after a reply is queued", replies: true, replyFirst: true},
		{name: "replies disabled: resolving needs no reply", replies: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &mutationRecorder{threads: []string{"PRRT_a"}}
			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: tc.replies},
				gqlClient: newTestGraphQLClient(t, recordMutations(rec)),
				findings:  []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"}},
			}
			cb := s.FindingCallbacks()
			if tc.replyFirst {
				if err := cb.Reply(t.Context(), "PRRT_a", "Fixed by bounding the read."); err != nil {
					t.Fatalf("Reply: %v", err)
				}
			}
			err := cb.Resolve(t.Context(), "PRRT_a")
			s.flushThreadActions(t.Context(), true)
			resolved := slices.Contains(rec.calls, "resolve:PRRT_a")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Resolve error: got = %v, want containing %q", err, tc.wantErr)
				}
				if resolved {
					t.Errorf("a refused resolution reached GitHub: %v", rec.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !resolved {
				t.Errorf("the resolution never reached GitHub: %v", rec.calls)
			}
		})
	}
}

// TestFlushThreadActionsResolvesOnlyRepliedThreads proves the flush itself
// gates a resolution on a posted reply when replies are enabled: a resolution
// queued with no reply on its thread, however it got there, does not reach
// GitHub. Without replies the pushed commit is the disposition and the
// resolution proceeds.
func TestFlushThreadActionsResolvesOnlyRepliedThreads(t *testing.T) {
	tests := []struct {
		name      string
		replies   bool
		wantCalls []string
	}{
		{name: "replies enabled: a resolution with no reply is not applied", replies: true},
		{name: "replies disabled: the commit is the disposition", replies: false, wantCalls: []string{"resolve:PRRT_a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &mutationRecorder{threads: []string{"PRRT_a"}}
			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: tc.replies},
				gqlClient: newTestGraphQLClient(t, recordMutations(rec)),
				threads:   &threadActions{},
			}
			// Queued directly, bypassing the callback's own check.
			s.threads.addResolve("PRRT_a")
			s.flushThreadActions(t.Context(), true)
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("mutations: got = %v, want = %v", rec.calls, tc.wantCalls)
			}
		})
	}
}

// TestFlushThreadActionsRetractsReplyWhenResolveFails proves a thread whose
// disposition reply posted but whose resolution failed is not left open with
// the bot's reply last, a state the next pass reads as settled and so never
// revisits: once the thread's state confirms it is still open, the reply is
// retracted so the thread awaits the bot again and the finding resurfaces. The
// reply stays when the read-back shows the resolution landed despite the error,
// or when the state cannot be read, since retracting it from a resolved thread
// would leave the thread closed with nothing to say why. An unrelated thread is
// still resolved, and a retraction that fails in turn is only logged.
func TestFlushThreadActionsRetractsReplyWhenResolveFails(t *testing.T) {
	tests := []struct {
		name      string
		fail      []string
		resolved  bool
		wantCalls []string
	}{
		{
			name:      "the resolution fails and the thread is open: its reply is retracted",
			fail:      []string{"resolve:PRRT_a"},
			wantCalls: []string{"reply:PRRT_a", "reply:PRRT_b", "resolve-failed:PRRT_a", "state:PRRT_a", "delete:PRRT_a", "resolve:PRRT_b"},
		},
		{
			name:      "the resolution reports an error but landed: the reply stays",
			fail:      []string{"resolve:PRRT_a"},
			resolved:  true,
			wantCalls: []string{"reply:PRRT_a", "reply:PRRT_b", "resolve-failed:PRRT_a", "state:PRRT_a", "resolve:PRRT_b"},
		},
		{
			name:      "the thread's state cannot be read: the reply stays",
			fail:      []string{"resolve:PRRT_a", "state:PRRT_a"},
			wantCalls: []string{"reply:PRRT_a", "reply:PRRT_b", "resolve-failed:PRRT_a", "state-failed:PRRT_a", "resolve:PRRT_b"},
		},
		{
			name:      "the retraction fails as well: the other thread is still resolved",
			fail:      []string{"resolve:PRRT_a", "delete:PRRT_a"},
			wantCalls: []string{"reply:PRRT_a", "reply:PRRT_b", "resolve-failed:PRRT_a", "state:PRRT_a", "delete-failed:PRRT_a", "resolve:PRRT_b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &mutationRecorder{threads: []string{"PRRT_a", "PRRT_b"}, resolved: tc.resolved}
			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: true},
				gqlClient: newTestGraphQLClient(t, failMutations(rec, tc.fail...)),
				findings: []callbacks.Finding{
					{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
					{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
				},
			}
			recordFixedThreads(t, s.FindingCallbacks(), "PRRT_a", "PRRT_b")
			s.flushThreadActions(t.Context(), true)
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("mutations: got = %v, want = %v", rec.calls, tc.wantCalls)
			}
		})
	}
}

// TestFlushThreadActionsWhenReplyPayloadOmitsComment proves a reply the
// mutation accepted still counts as posted when the payload carries no comment
// id: the resolution proceeds, since the disposition is on the thread, and if
// the resolution then fails there is nothing to retract, so no delete is sent
// with an empty id. Reporting the missing id as a failed reply would instead skip
// the resolution and leave the thread settled with the bot's reply last.
func TestFlushThreadActionsWhenReplyPayloadOmitsComment(t *testing.T) {
	tests := []struct {
		name      string
		fail      []string
		wantCalls []string
	}{
		{name: "the resolution proceeds", wantCalls: []string{"reply:PRRT_a", "resolve:PRRT_a"}},
		{name: "a failed resolution sends no retraction", fail: []string{"resolve:PRRT_a"}, wantCalls: []string{"reply:PRRT_a", "resolve-failed:PRRT_a", "state:PRRT_a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &mutationRecorder{threads: []string{"PRRT_a"}, omitCommentID: true}
			s := &Session[testData]{
				manager:   &CM[testData]{findingReplies: true},
				gqlClient: newTestGraphQLClient(t, failMutations(rec, tc.fail...)),
				findings:  []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"}},
			}
			recordFixedThreads(t, s.FindingCallbacks(), "PRRT_a")
			s.flushThreadActions(t.Context(), true)
			if !slices.Equal(rec.calls, tc.wantCalls) {
				t.Errorf("mutations: got = %v, want = %v", rec.calls, tc.wantCalls)
			}
		})
	}
}

// TestThreadActionScopeDiscardsOnlyLaterActions proves a discarded scope drops
// exactly the actions queued after it began: a reverted pass's replies and
// resolutions go, an earlier pass's stay, and nesting is respected.
func TestThreadActionScopeDiscardsOnlyLaterActions(t *testing.T) {
	rec := &mutationRecorder{threads: []string{"PRRT_a", "PRRT_b", "PRRT_c"}}
	s := &Session[testData]{
		manager:   &CM[testData]{findingReplies: true},
		gqlClient: newTestGraphQLClient(t, recordMutations(rec)),
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_c"},
		},
	}
	cb := s.FindingCallbacks()
	must := func(err error) {
		if err != nil {
			t.Fatalf("recording thread action: %v", err)
		}
	}
	// Pass 1 completes.
	must(cb.Reply(t.Context(), "PRRT_a", "Fixed."))
	must(cb.Resolve(t.Context(), "PRRT_a"))
	// Pass 2 is reverted: its actions must go with its edits, and discarding
	// twice changes nothing more.
	discard := s.BeginThreadActionScope()
	must(cb.Reply(t.Context(), "PRRT_b", "Fixed."))
	must(cb.Resolve(t.Context(), "PRRT_b"))
	discard()
	discard()
	// Pass 3 completes: its scope is never discarded, so its actions stay.
	_ = s.BeginThreadActionScope()
	must(cb.Reply(t.Context(), "PRRT_c", "Fixed."))
	must(cb.Resolve(t.Context(), "PRRT_c"))

	s.flushThreadActions(t.Context(), true)
	want := []string{"reply:PRRT_a", "reply:PRRT_c", "resolve:PRRT_a", "resolve:PRRT_c"}
	if !slices.Equal(rec.calls, want) {
		t.Errorf("mutations: got = %v, want = %v", rec.calls, want)
	}
}
