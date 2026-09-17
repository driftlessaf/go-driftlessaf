/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
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
// review-thread mutation each request carried and for which known thread.
type mutationRecorder struct {
	threads []string
	calls   []string
}

// recordMutations returns the recorder's handler.
func recordMutations(m *mutationRecorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		kind := "other"
		switch {
		case strings.Contains(body, "addPullRequestReviewThreadReply"):
			kind = "reply"
		case strings.Contains(body, "resolveReviewThread"):
			kind = "resolve"
		}
		thread := "?"
		for _, id := range m.threads {
			if strings.Contains(body, id) {
				thread = id
			}
		}
		m.calls = append(m.calls, kind+":"+thread)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{}}`)
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
	inner := recordMutations(rec)
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		r.Body = io.NopCloser(strings.NewReader(body))
		if strings.Contains(body, "addPullRequestReviewThreadReply") && strings.Contains(body, "PRRT_a") {
			rec.calls = append(rec.calls, "reply-failed:PRRT_a")
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	})
	s := &Session[testData]{
		manager:   &CM[testData]{findingReplies: true},
		gqlClient: newTestGraphQLClient(t, failing),
		findings: []callbacks.Finding{
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_a"},
			{Kind: callbacks.FindingKindReview, Identifier: "PRRT_b"},
		},
	}
	cb := s.FindingCallbacks()
	for _, step := range []func() error{
		func() error { return cb.Reply(t.Context(), "PRRT_a", "Fixed by bounding the read.") },
		func() error { return cb.Resolve(t.Context(), "PRRT_a") },
		func() error { return cb.Reply(t.Context(), "PRRT_b", "Fixed by checking the error.") },
		func() error { return cb.Resolve(t.Context(), "PRRT_b") },
	} {
		if err := step(); err != nil {
			t.Fatalf("recording thread action: %v", err)
		}
	}
	s.flushThreadActions(t.Context(), true)
	want := []string{"reply-failed:PRRT_a", "reply:PRRT_b", "resolve:PRRT_b"}
	if !slices.Equal(rec.calls, want) {
		t.Errorf("mutations: got = %v, want = %v (no resolve for the thread whose reply failed)", rec.calls, want)
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
