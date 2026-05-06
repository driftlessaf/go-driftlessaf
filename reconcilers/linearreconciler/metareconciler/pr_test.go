/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/google/go-github/v84/github"
)

// Trivial test types to satisfy the Reconciler's generic constraints.
// Dispatch doesn't actually use Req/Resp/CB so these can be empty.

type testReq = promptbuilder.Noop

type testResp struct{}

func (testResp) GetCommitMessage() string { return "" }

type testCB struct{}

// linearStateFixture is a small mock of the bits of Linear's GraphQL API +
// asset CDN that the state-mutation helpers exercise: fetch issue, load
// state JSON, upload new state, register the new attachment, delete the
// old one.
//
// `lastSavedState` captures the body of the upload PUT so tests can assert
// the persisted JSON without poking at Linear under test.
type linearStateFixture struct {
	server         *httptest.Server
	issue          *linearreconciler.Issue
	lastSavedState atomic.Pointer[[]byte]
	saveCount      atomic.Int32

	// callOrder records the sequence of side-effecting operations the
	// fixture observes. "comment" appended on commentCreate/commentUpdate
	// GraphQL mutations, "save" appended on the upload PUT. Lets tests
	// assert ordering invariants between UpsertBotComment and Save.
	callOrderMu sync.Mutex
	callOrder   []string
}

func (f *linearStateFixture) recordCall(name string) {
	f.callOrderMu.Lock()
	defer f.callOrderMu.Unlock()
	f.callOrder = append(f.callOrder, name)
}

func (f *linearStateFixture) getCallOrder() []string {
	f.callOrderMu.Lock()
	defer f.callOrderMu.Unlock()
	out := make([]string, len(f.callOrder))
	copy(out, f.callOrder)
	return out
}

func newLinearStateFixture(t *testing.T, initialStateJSON string) *linearStateFixture {
	t.Helper()
	f := &linearStateFixture{}

	mux := http.NewServeMux()
	mux.HandleFunc("/state.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(initialStateJSON))
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bcopy := append([]byte(nil), body...)
		f.lastSavedState.Store(&bcopy)
		f.saveCount.Add(1)
		f.recordCall("save")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Everything else is treated as a Linear GraphQL request.
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var data any
		// Match more-specific mutations before generic substring checks
		// like "issue" (which would also match "issueId" parameters).
		switch {
		case strings.Contains(req.Query, "commentCreate"):
			f.recordCall("comment")
			data = map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": "comment-1"}}}
		case strings.Contains(req.Query, "commentUpdate"):
			f.recordCall("comment")
			data = map[string]any{"commentUpdate": map[string]any{"success": true, "comment": map[string]any{"id": "comment-1"}}}
		case strings.Contains(req.Query, "viewer"):
			data = map[string]any{"viewer": map[string]any{"id": "bot-1", "name": "Test Bot"}}
		case strings.Contains(req.Query, "fileUpload"):
			data = map[string]any{"fileUpload": map[string]any{"uploadFile": map[string]any{
				"uploadUrl": f.server.URL + "/upload",
				"assetUrl":  "https://uploads.linear.app/test/asset",
				"headers":   []any{},
			}}}
		case strings.Contains(req.Query, "attachmentDelete"):
			data = map[string]any{"attachmentDelete": map[string]any{"success": true}}
		case strings.Contains(req.Query, "attachmentCreate"):
			data = map[string]any{"attachmentCreate": map[string]any{"success": true}}
		case strings.Contains(req.Query, "issue"):
			// Lazy: f.issue is set below after the server URL is known.
			data = map[string]any{"issue": f.issue}
		default:
			data = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	// The issue's attachment URL needs the server URL, which only exists
	// post-construction — populate after the server is up.
	f.issue = &linearreconciler.Issue{
		ID:         "issue-1",
		Identifier: "TEST-1",
		Title:      "Materializer-managed test issue",
	}
	f.issue.Attachments.Nodes = []linearreconciler.Attachment{
		{ID: "att-1", Title: "materializer_state", URL: f.server.URL + "/state.json"},
	}
	return f
}

// newClient builds a Linear client wired through the fixture's mock endpoint
// with the materializer state-prefix set (so attachments titled
// "materializer_state" route correctly).
func (f *linearStateFixture) newClient(t *testing.T) *linearreconciler.Client {
	t.Helper()
	client := linearreconciler.NewClientWithAPIKey("test-token").
		WithHTTPClient(f.server.Client()).
		WithEndpoint(f.server.URL)
	// linearreconciler.New is the only public path that sets statePrefix on
	// the Client; we use it for its side-effect and discard the Reconciler.
	if _, err := linearreconciler.New(t.Context(), client, linearreconciler.WithStatePrefix("materializer")); err != nil {
		t.Fatalf("init linearreconciler: %v", err)
	}
	return client
}

// TestDispatchMergedOrRequeue exercises the three-way decision in
// (*Reconciler).dispatchMergedOrRequeue: a merged PR must persist
// StatusComplete, a closed-without-merge PR must persist StatusFailed/pr_closed,
// and an open PR must re-queue without touching state.
func TestDispatchMergedOrRequeue(t *testing.T) {
	const prURL = "https://github.com/o/r/pull/1"

	tests := []struct {
		name            string
		merged          bool
		state           string // GitHub PR state: "open" or "closed"
		wantRequeueKeys int
		wantSaveCount   int32
		wantStatusJSON  string // substring expected in the saved state, if any save happened
	}{
		{
			name:            "merged → mark complete, no requeue",
			merged:          true,
			state:           "closed", // merged PRs are also closed
			wantRequeueKeys: 0,
			wantSaveCount:   1,
			wantStatusJSON:  `"status":"complete"`,
		},
		{
			// Defensive case: GitHub never returns merged=true with state=open
			// in practice, but locking this in guards against future refactors
			// that could reorder the merged/closed branches in dispatchMergedOrRequeue.
			name:            "merged with state=open (defensive) → mark complete, no requeue",
			merged:          true,
			state:           "open",
			wantRequeueKeys: 0,
			wantSaveCount:   1,
			wantStatusJSON:  `"status":"complete"`,
		},
		{
			name:            "closed without merge → mark failed (pr_closed), no requeue",
			merged:          false,
			state:           "closed",
			wantRequeueKeys: 0,
			wantSaveCount:   1,
			wantStatusJSON:  `"status":"failed"`,
		},
		{
			name:            "open PR → requeue, no state change",
			merged:          false,
			state:           "open",
			wantRequeueKeys: 1,
			wantSaveCount:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinearStateFixture(t, `{"pr_url":"`+prURL+`","status":"active"}`)
			client := f.newClient(t)

			// Build a minimal Reconciler with just the fields dispatch uses.
			// The Req/Resp/CB type params are unused by dispatch; pick trivial
			// types just to satisfy generics. State type is the framework's
			// own State (no bot extensions needed for this test).
			r := &Reconciler[testReq, testResp, testCB, State, *State]{
				identity:     "test-bot",
				linearClient: client,
			}

			pr := &github.PullRequest{
				Merged: github.Ptr(tc.merged),
				State:  github.Ptr(tc.state),
			}
			resp, err := r.dispatchMergedOrRequeue(t.Context(), pr, f.issue.ID, prURL)
			if err != nil {
				t.Fatalf("dispatchMergedOrRequeue: %v", err)
			}

			if got := len(resp.QueueKeys); got != tc.wantRequeueKeys {
				t.Errorf("len(QueueKeys): got = %d, want = %d", got, tc.wantRequeueKeys)
			}
			if got := f.saveCount.Load(); got != tc.wantSaveCount {
				t.Errorf("save count: got = %d, want = %d", got, tc.wantSaveCount)
			}
			if tc.wantStatusJSON != "" {
				saved := string(*f.lastSavedState.Load())
				if !strings.Contains(saved, tc.wantStatusJSON) {
					t.Errorf("saved state: got = %q, want substring %q", saved, tc.wantStatusJSON)
				}
			}
		})
	}
}

// TestStateTypeConstants_MatchLinearEnum prevents a regression where the
// stateTypeCanceled constant carried British "cancelled" spelling while
// Linear's GraphQL API returns the American "canceled" — the gate at
// reconcileIssue:45 silently never fired for cancelled issues, so an
// issue cancelled in Linear could still trigger PR creation. Linear's
// WorkflowStateType enum is fixed; if this test fails, the constants
// have drifted from the API contract and the gate is broken again.
//
// See https://developers.linear.app/docs/graphql/working-with-the-graphql-api/workflow-states
func TestStateTypeConstants_MatchLinearEnum(t *testing.T) {
	cases := map[string]string{
		"completed": stateTypeCompleted,
		"canceled":  stateTypeCanceled,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("constant for Linear state %q: got = %q, want = %q (must match Linear's GraphQL WorkflowStateType enum exactly)", want, got, want)
		}
	}
}
