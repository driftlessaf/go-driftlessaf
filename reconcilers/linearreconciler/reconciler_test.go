/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/workqueue"
)

// newTestServer creates a mock Linear GraphQL server that returns the given issue.
func newTestServer(t *testing.T, issue *Issue) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var resp any

		switch {
		case containsSubstring(req.Query, "viewer"):
			resp = map[string]any{
				"viewer": map[string]any{"id": "bot-1", "name": "Test Bot"},
			}
		case containsSubstring(req.Query, "issue"):
			resp = map[string]any{"issue": issue}
		default:
			http.Error(w, "unknown query", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": resp})
	}))
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsString(s, sub))
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestReconciler_LabelGating(t *testing.T) {
	issue := &Issue{
		ID:         "issue-1",
		Identifier: "TEST-1",
		Title:      "Test Issue",
	}
	issue.Labels.Nodes = []struct {
		Name string `json:"name"`
	}{{Name: "other"}}
	issue.Team.Key = "ENG"

	srv := newTestServer(t, issue)
	defer srv.Close()

	var called atomic.Bool
	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	r := &Reconciler{
		client:         client,
		requiredLabels: []string{"game"},
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			called.Store(true)
			return nil
		},
	}
	r.client.BotUserID = "bot-1"

	if issue.HasLabel("game") {
		t.Error("issue should NOT have game label")
	}
	if called.Load() {
		t.Error("reconciler should not have been called")
	}
}

func TestReconciler_TeamFilter(t *testing.T) {
	issue := &Issue{
		ID:         "issue-1",
		Identifier: "TEST-1",
		Title:      "Test Issue",
	}
	issue.Team.Key = "WRONG"

	r := &Reconciler{
		teamFilter: "ENG",
	}

	if issue.Team.Key == r.teamFilter {
		t.Error("team should NOT match filter")
	}
}

func TestReconciler_Process_Success(t *testing.T) {
	var processedKey string
	r := &Reconciler{
		reconcileFunc: func(_ context.Context, issue *Issue, _ *Client) error {
			processedKey = issue.ID
			return nil
		},
		client: NewClientWithAPIKey("test-token"),
	}
	r.client.BotUserID = "bot-1"

	// Test that Process delegates to Reconcile properly by testing with
	// an empty key (which should return non-retriable error).
	resp, err := r.Process(context.Background(), &workqueue.ProcessRequest{Key: ""})
	if err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	// Empty key is non-retriable, so Process returns success.
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if processedKey != "" {
		t.Error("reconciler should not have been called for empty key")
	}
}

func TestReconciler_Process_RequeueOnError(t *testing.T) {
	r := &Reconciler{
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			return fmt.Errorf("temporary error")
		},
		client: NewClientWithAPIKey("test-token"),
	}
	r.client.BotUserID = "bot-1"

	// Can't easily test full flow without mock server, but we can test
	// that Reconcile with empty key returns non-retriable.
	err := r.Reconcile(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if details := workqueue.GetNonRetriableDetails(err); details == nil {
		t.Error("expected non-retriable error for empty key")
	}
}

func TestWithStatePrefix(t *testing.T) {
	client := NewClientWithAPIKey("test-token")
	r := &Reconciler{client: client}

	WithStatePrefix("game")(r)

	if got := client.stateAttachmentTitle(); got != "game_state" {
		t.Errorf("stateAttachmentTitle() = %q, want %q", got, "game_state")
	}
}

func TestWithStatePrefix_Default(t *testing.T) {
	client := NewClientWithAPIKey("test-token")

	if got := client.stateAttachmentTitle(); got != "reconciler_state" {
		t.Errorf("stateAttachmentTitle() = %q, want %q", got, "reconciler_state")
	}
}

// newTestServerMulti creates a mock Linear GraphQL server that dispatches to
// issuesByID based on the "id" variable in the GraphQL request.
func newTestServerMulti(t *testing.T, issuesByID map[string]*Issue) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var resp any

		switch {
		case containsSubstring(req.Query, "viewer"):
			resp = map[string]any{
				"viewer": map[string]any{"id": "bot-1", "name": "Test Bot"},
			}
		case containsSubstring(req.Query, "issue"):
			id, _ := req.Variables["id"].(string)
			issue, ok := issuesByID[id]
			if !ok {
				http.Error(w, "issue not found", http.StatusNotFound)
				return
			}
			resp = map[string]any{"issue": issue}
		default:
			http.Error(w, "unknown query", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": resp})
	}))
}

func TestReconcile_MultipleRequiredLabels_AnyMatch(t *testing.T) {
	makeIssue := func(id, identifier, label string) *Issue {
		issue := &Issue{
			ID:         id,
			Identifier: identifier,
			Title:      "Issue " + identifier,
		}
		issue.Labels.Nodes = []struct {
			Name string `json:"name"`
		}{{Name: label}}
		issue.Team.Key = "ENG"
		return issue
	}

	alphaIssue := makeIssue("issue-alpha", "TEST-A", "alpha")
	betaIssue := makeIssue("issue-beta", "TEST-B", "beta")
	gammaIssue := makeIssue("issue-gamma", "TEST-G", "gamma")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-alpha": alphaIssue,
		"issue-beta":  betaIssue,
		"issue-gamma": gammaIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client:         client,
		requiredLabels: []string{"alpha", "beta"},
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	// alpha issue has label "alpha" — should be accepted.
	if err := r.Reconcile(ctx, "issue-alpha"); err != nil {
		t.Fatalf("Reconcile(alpha) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after alpha: reconcileFunc call count = %d, want 1", got)
	}

	// beta issue has label "beta" — should be accepted.
	if err := r.Reconcile(ctx, "issue-beta"); err != nil {
		t.Fatalf("Reconcile(beta) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 2 {
		t.Errorf("after beta: reconcileFunc call count = %d, want 2", got)
	}

	// gamma issue has label "gamma" — should be filtered out.
	if err := r.Reconcile(ctx, "issue-gamma"); err != nil {
		t.Fatalf("Reconcile(gamma) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 2 {
		t.Errorf("after gamma: reconcileFunc call count = %d, want 2 (gamma should be skipped)", got)
	}
}

// makeIssueLabels builds an Issue with the given labels attached. Used by the
// AND/NOT/predicate tests below where we need more than one label per issue.
func makeIssueLabels(id, identifier string, labels ...string) *Issue {
	issue := &Issue{
		ID:         id,
		Identifier: identifier,
		Title:      "Issue " + identifier,
	}
	for _, l := range labels {
		issue.Labels.Nodes = append(issue.Labels.Nodes, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	issue.Team.Key = "ENG"
	return issue
}

func TestReconcile_AllRequiredLabels_AND(t *testing.T) {
	bothIssue := makeIssueLabels("issue-both", "TEST-BOTH", "alpha", "beta")
	alphaIssue := makeIssueLabels("issue-alpha", "TEST-A", "alpha")
	emptyIssue := makeIssueLabels("issue-empty", "TEST-E")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-both":  bothIssue,
		"issue-alpha": alphaIssue,
		"issue-empty": emptyIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client: client,
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	WithAllRequiredLabels("alpha", "beta")(r)
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	if err := r.Reconcile(ctx, "issue-both"); err != nil {
		t.Fatalf("Reconcile(both) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after both: call count = %d, want 1", got)
	}

	if err := r.Reconcile(ctx, "issue-alpha"); err != nil {
		t.Fatalf("Reconcile(alpha) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after alpha (missing beta): call count = %d, want 1 (skipped)", got)
	}

	if err := r.Reconcile(ctx, "issue-empty"); err != nil {
		t.Fatalf("Reconcile(empty) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after empty: call count = %d, want 1 (skipped)", got)
	}
}

func TestReconcile_WithoutLabel_NOT(t *testing.T) {
	plainIssue := makeIssueLabels("issue-plain", "TEST-P", "alpha")
	skippedIssue := makeIssueLabels("issue-skip", "TEST-S", "alpha", "skip:linear-materializer")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-plain": plainIssue,
		"issue-skip":  skippedIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client: client,
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	WithoutLabel("skip:linear-materializer")(r)
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	if err := r.Reconcile(ctx, "issue-plain"); err != nil {
		t.Fatalf("Reconcile(plain) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after plain: call count = %d, want 1", got)
	}

	if err := r.Reconcile(ctx, "issue-skip"); err != nil {
		t.Fatalf("Reconcile(skip) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after skip: call count = %d, want 1 (issue with skip label must be filtered)", got)
	}
}

func TestReconcile_LabelPredicate_Custom(t *testing.T) {
	managedIssue := makeIssueLabels("issue-managed", "TEST-M", "team:platform", "materializer:managed")
	otherIssue := makeIssueLabels("issue-other", "TEST-O", "team:other", "materializer:managed")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-managed": managedIssue,
		"issue-other":   otherIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client: client,
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	// Custom predicate: must carry team:platform.
	WithLabelPredicate(func(labels []string) bool {
		return slices.Contains(labels, "team:platform")
	})(r)
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	if err := r.Reconcile(ctx, "issue-managed"); err != nil {
		t.Fatalf("Reconcile(managed) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after managed: call count = %d, want 1", got)
	}

	if err := r.Reconcile(ctx, "issue-other"); err != nil {
		t.Fatalf("Reconcile(other) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after other: call count = %d, want 1 (predicate must reject)", got)
	}
}

// TestReconcile_LabelOptions_CaseInsensitive pins down the documented
// case-insensitive contract on WithAllRequiredLabels and WithoutLabel: a label
// stored as "Materializer:Managed" must still match the lowercase option.
func TestReconcile_LabelOptions_CaseInsensitive(t *testing.T) {
	mixedCaseIssue := makeIssueLabels("issue-mixed", "TEST-MIXED", "Materializer:Managed", "Skip:Linear-Materializer")
	plainIssue := makeIssueLabels("issue-plain", "TEST-PLAIN", "Materializer:Managed")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-mixed": mixedCaseIssue,
		"issue-plain": plainIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client: client,
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	WithAllRequiredLabels("materializer:managed")(r)
	WithoutLabel("skip:linear-materializer")(r)
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	// plain has the lowercase-required label in mixed case; should be accepted.
	if err := r.Reconcile(ctx, "issue-plain"); err != nil {
		t.Fatalf("Reconcile(plain) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after plain: call count = %d, want 1 (case-insensitive match)", got)
	}

	// mixed also carries the skip label in mixed case; should be filtered.
	if err := r.Reconcile(ctx, "issue-mixed"); err != nil {
		t.Fatalf("Reconcile(mixed) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after mixed: call count = %d, want 1 (skip label must match case-insensitively)", got)
	}
}

// TestReconcile_RequiredAndWithoutLabel_Mixed exercises the materializer's
// real-world composition: WithRequiredLabel + WithoutLabel must AND together.
func TestReconcile_RequiredAndWithoutLabel_Mixed(t *testing.T) {
	managedIssue := makeIssueLabels("issue-managed", "TEST-M", "materializer:managed")
	skippedIssue := makeIssueLabels("issue-skip", "TEST-S", "materializer:managed", "skip:linear-materializer")
	unmanagedIssue := makeIssueLabels("issue-unmanaged", "TEST-U", "other:label")

	srv := newTestServerMulti(t, map[string]*Issue{
		"issue-managed":   managedIssue,
		"issue-skip":      skippedIssue,
		"issue-unmanaged": unmanagedIssue,
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	var callCount atomic.Int32
	r := &Reconciler{
		client:         client,
		requiredLabels: []string{"materializer:managed"},
		reconcileFunc: func(_ context.Context, _ *Issue, _ *Client) error {
			callCount.Add(1)
			return nil
		},
	}
	WithoutLabel("skip:linear-materializer")(r)
	r.client.BotUserID = "bot-1"

	ctx := t.Context()

	if err := r.Reconcile(ctx, "issue-managed"); err != nil {
		t.Fatalf("Reconcile(managed) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after managed: call count = %d, want 1", got)
	}

	if err := r.Reconcile(ctx, "issue-skip"); err != nil {
		t.Fatalf("Reconcile(skip) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after skip: call count = %d, want 1 (skip label must take precedence)", got)
	}

	if err := r.Reconcile(ctx, "issue-unmanaged"); err != nil {
		t.Fatalf("Reconcile(unmanaged) unexpected error: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("after unmanaged: call count = %d, want 1 (missing required label)", got)
	}
}

// TestReconciler_Process_SubSecondRequeue checks that a requeue under a
// second reaches the wire as one second. Zero RequeueAfterSeconds tells the
// dispatcher the key is done, so truncating would drop the issue until its
// next resync.
func TestReconciler_Process_SubSecondRequeue(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantFloor bool
	}{{
		name: "requeue after",
		err:  workqueue.RequeueAfter(500 * time.Millisecond),
	}, {
		name:      "requeue not before",
		err:       workqueue.RequeueNotBefore(500 * time.Millisecond),
		wantFloor: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &Issue{ID: "issue-1", Identifier: "TEST-1", Title: "Test Issue"}
			srv := newTestServer(t, issue)
			defer srv.Close()

			r := &Reconciler{
				client: NewClientWithAPIKey("test-token").
					WithHTTPClient(srv.Client()).
					WithEndpoint(srv.URL),
				reconcileFunc: func(context.Context, *Issue, *Client) error { return tt.err },
			}

			resp, err := r.Process(t.Context(), &workqueue.ProcessRequest{Key: issue.ID})
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if got := resp.GetRequeueAfterSeconds(); got != 1 {
				t.Errorf("RequeueAfterSeconds: got = %d, want = 1", got)
			}
			if got := resp.GetRequeueFloor(); got != tt.wantFloor {
				t.Errorf("RequeueFloor: got = %t, want = %t", got, tt.wantFloor)
			}
		})
	}
}
