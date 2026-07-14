/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/google/go-github/v88/github"
	"golang.org/x/oauth2"
)

// Trivial test types to satisfy the Reconciler's generic constraints.
// Dispatch doesn't actually use Req/Resp/CB so these can be empty.

type testReq = promptbuilder.Noop

type testResp struct{}

func (testResp) GetCommitMessage() string { return "" }

type testCB struct{}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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

	// failSaves makes the /upload handler return 500, simulating a Linear
	// attachment-save failure. Tests use it to assert that side effects
	// gated on a successful save (transition CloudEvents) do not fire.
	failSaves atomic.Bool

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
		if f.failSaves.Load() {
			http.Error(w, "simulated save failure", http.StatusInternalServerError)
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
		URL:        "https://linear.app/test/issue/TEST-1",
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

// TestMaybeTransitionFromPRState exercises the merged/closed/open
// decision in (*Reconciler).maybeTransitionFromPRState. This is the
// linear-workqueue-side helper that replaced the github-workqueue's
// transitionPR — same per-case behaviour, but the Save now happens
// inside reconcileIssue so it serializes with every other write to this
// Linear issue's State.
func TestMaybeTransitionFromPRState(t *testing.T) {
	const prURL = "https://github.com/o/r/pull/1"

	tests := []struct {
		name           string
		merged         bool
		state          string // GitHub PR state: "open" or "closed"
		wantDone       bool
		wantSaveCount  int32
		wantStatusJSON string // substring expected in the saved state, if any save happened
	}{
		{
			name:           "merged → mark complete, done",
			merged:         true,
			state:          "closed", // merged PRs are also closed in GitHub's model
			wantDone:       true,
			wantSaveCount:  1,
			wantStatusJSON: `"status":"complete"`,
		},
		{
			// Defensive case: GitHub never returns merged=true with state=open
			// in practice, but locking this in guards against future refactors
			// that could reorder the merged/closed branches.
			name:           "merged with state=open (defensive) → mark complete, done",
			merged:         true,
			state:          "open",
			wantDone:       true,
			wantSaveCount:  1,
			wantStatusJSON: `"status":"complete"`,
		},
		{
			name:           "closed without merge → mark failed (pr_closed), done",
			merged:         false,
			state:          "closed",
			wantDone:       true,
			wantSaveCount:  1,
			wantStatusJSON: `"status":"failed"`,
		},
		{
			name:          "open PR → no transition, fall through (caller continues normal flow)",
			merged:        false,
			state:         "open",
			wantDone:      false,
			wantSaveCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinearStateFixture(t, `{"pr_url":"`+prURL+`","status":"active"}`)
			r := newReconcilerForFixture(t, f)
			mgr := r.NewStateManager(f.issue)
			existing, _, err := mgr.Load(t.Context())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			pr := &github.PullRequest{
				Merged: github.Ptr(tc.merged),
				State:  github.Ptr(tc.state),
			}
			done, err := r.maybeTransitionFromPRState(t.Context(), mgr, existing, pr)
			if err != nil {
				t.Fatalf("maybeTransitionFromPRState: %v", err)
			}
			if done != tc.wantDone {
				t.Errorf("done: got = %v, want = %v", done, tc.wantDone)
			}
			if got := f.saveCount.Load(); got != tc.wantSaveCount {
				t.Errorf("save count: got = %d, want = %d", got, tc.wantSaveCount)
			}
			if tc.wantStatusJSON != "" {
				saved := string(*f.lastSavedState.Load())
				if !strings.Contains(saved, tc.wantStatusJSON) {
					t.Errorf("saved state: got = %q, want substring %q", saved, tc.wantStatusJSON)
				}
				// Lock in PRURL preservation through the terminal Save: the
				// old transitionPR path had explicit PRURL-backfill logic
				// that maybeTransitionFromPRState dropped. If a future
				// refactor accidentally clears PRURL on the terminal write,
				// downstream consumers would lose the link to the closed/
				// merged PR.
				if !strings.Contains(saved, prURL) {
					t.Errorf("saved state lost PRURL on terminal transition: got = %q, want substring %q", saved, prURL)
				}
			}
		})
	}
}

// TestHandlePREvent_RoutingOnlyDoesNotWriteState locks in the load-bearing
// invariant of this refactor: HandlePREvent MUST NOT write State,
// regardless of what the github fetch returns. Pre-refactor the function
// (then transitionPR) wrote State directly from the github workqueue,
// racing with reconcileIssue's writes on the linear workqueue. The whole
// point of moving terminal-transition logic into reconcileIssue was to
// eliminate that race; if a future contributor adds a Save call back into
// HandlePREvent, this test fires immediately rather than reproducing the
// stale-mirror bug in production.
//
// Table-driven across the routing decisions HandlePREvent makes against
// the PR body: marker-present + LinearIssueID populated (the success
// path), marker-present + empty LinearIssueID (skip-with-warn),
// marker-absent (skip-with-info). Each case must produce zero State
// writes; the success path additionally must enqueue the LinearIssueID.
func TestHandlePREvent_RoutingOnlyDoesNotWriteState(t *testing.T) {
	const (
		prURL         = "https://github.com/o/r/pull/1"
		linearIssueID = "issue-1"
	)

	// Marker format mirrors internal/template's Embed:
	//   <!--{identity}-pr-data-->
	//   <!--
	//   {json}
	//   -->
	//   <!--/{identity}-pr-data-->
	prBodyWithID := fmt.Sprintf("Some body.\n\n<!--%s-pr-data-->\n<!--\n%s\n-->\n<!--/%s-pr-data-->",
		testActor,
		`{"identity":"`+testActor+`","linear_issue_id":"`+linearIssueID+`","linear_identifier":"TEST-1","description_hash":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}`,
		testActor,
	)
	prBodyEmptyID := fmt.Sprintf("Some body.\n\n<!--%s-pr-data-->\n<!--\n%s\n-->\n<!--/%s-pr-data-->",
		testActor,
		`{"identity":"`+testActor+`","linear_issue_id":"","linear_identifier":"","description_hash":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}`,
		testActor,
	)
	prBodyNoMarker := "A perfectly normal PR body with no materializer marker."

	tests := []struct {
		name           string
		body           string
		state          string
		merged         bool
		wantQueueCount int
		wantQueueKey   string
	}{
		{
			name:           "marker present, LinearIssueID set, PR closed → enqueue, no State write",
			body:           prBodyWithID,
			state:          "closed",
			wantQueueCount: 1,
			wantQueueKey:   linearIssueID,
		},
		{
			name:           "marker present, LinearIssueID set, PR merged → enqueue, no State write",
			body:           prBodyWithID,
			state:          "closed",
			merged:         true,
			wantQueueCount: 1,
			wantQueueKey:   linearIssueID,
		},
		{
			name:           "marker present, LinearIssueID set, PR open → enqueue, no State write",
			body:           prBodyWithID,
			state:          "open",
			wantQueueCount: 1,
			wantQueueKey:   linearIssueID,
		},
		{
			name:           "marker present, empty LinearIssueID → skip, no State write",
			body:           prBodyEmptyID,
			state:          "closed",
			wantQueueCount: 0,
		},
		{
			name:           "no marker → skip, no State write",
			body:           prBodyNoMarker,
			state:          "closed",
			wantQueueCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinearStateFixture(t, `{"pr_url":"`+prURL+`","status":"active"}`)

			// Stand up a minimal GitHub mock returning a PR with the
			// given body. The fetch is the only github call HandlePREvent
			// makes — overriding the cached client's BaseURL is the
			// least-invasive way to redirect it.
			ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":     1,
					"number": 1,
					"state":  tc.state,
					"merged": tc.merged,
					"body":   tc.body,
				})
			}))
			t.Cleanup(ghServer.Close)

			cc := githubreconciler.NewClientCache(func(_ context.Context, _, _ string) (oauth2.TokenSource, error) {
				return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), nil
			})
			// go-github v88 made Client.BaseURL read-only, so the old approach
			// of overriding it on the cached client no longer compiles. Instead,
			// pre-warm the cache with a context carrying an HTTP client whose
			// transport rewrites every request to the mock server; the cache
			// builds its oauth2 client on top of that base, and the reconciler's
			// own cc.Get returns the same cached, redirected client.
			mockURL, err := url.Parse(ghServer.URL)
			if err != nil {
				t.Fatalf("parse mock URL: %v", err)
			}
			redirect := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				req.URL.Scheme = mockURL.Scheme
				req.URL.Host = mockURL.Host
				return http.DefaultTransport.RoundTrip(req)
			})}
			warmCtx := context.WithValue(t.Context(), oauth2.HTTPClient, redirect)
			if _, err := cc.Get(warmCtx, "o", "r"); err != nil {
				t.Fatalf("ClientCache.Get: %v", err)
			}

			titleTmpl, err := template.New("title").Parse("test title")
			if err != nil {
				t.Fatalf("title template: %v", err)
			}
			bodyTmpl, err := template.New("body").Parse("test body")
			if err != nil {
				t.Fatalf("body template: %v", err)
			}
			cm, err := changemanager.New[PRData[testReq]](testActor, titleTmpl, bodyTmpl)
			if err != nil {
				t.Fatalf("changemanager.New: %v", err)
			}

			r := newReconcilerForFixture(t, f)
			r.githubClients = cc
			r.changeManager = cm

			resp, err := r.HandlePREvent(t.Context(), prURL)
			if err != nil {
				t.Fatalf("HandlePREvent: %v", err)
			}

			if got := len(resp.QueueKeys); got != tc.wantQueueCount {
				t.Errorf("QueueKeys count: got = %d, want = %d", got, tc.wantQueueCount)
			}
			if tc.wantQueueKey != "" && len(resp.QueueKeys) > 0 {
				if got := resp.QueueKeys[0].Key; got != tc.wantQueueKey {
					t.Errorf("queue key: got = %q, want = %q", got, tc.wantQueueKey)
				}
			}
			// The headline assertion: HandlePREvent is purely a router and
			// must not touch State for ANY input. Cross-workqueue race
			// regression caught here, not in production.
			if got := f.saveCount.Load(); got != 0 {
				t.Errorf("State saveCount: got = %d, want = 0 (HandlePREvent must be routing-only)", got)
			}
		})
	}
}

// TestStateTypeConstants_MatchLinearEnum prevents a regression where the
// StateTypeCanceled constant carried British "cancelled" spelling while
// Linear's GraphQL API returns the American "canceled" — the gate at
// reconcileIssue:45 silently never fired for cancelled issues, so an
// issue cancelled in Linear could still trigger PR creation. Linear's
// WorkflowStateType enum is fixed; if this test fails, the constants
// have drifted from the API contract and the gate is broken again.
//
// See https://developers.linear.app/docs/graphql/working-with-the-graphql-api/workflow-states
func TestStateTypeConstants_MatchLinearEnum(t *testing.T) {
	cases := map[string]string{
		"completed": StateTypeCompleted,
		"canceled":  StateTypeCanceled,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("constant for Linear state %q: got = %q, want = %q (must match Linear's GraphQL WorkflowStateType enum exactly)", want, got, want)
		}
	}
}
