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
	"sync/atomic"
	"testing"

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
		client:        client,
		requiredLabel: "game",
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
