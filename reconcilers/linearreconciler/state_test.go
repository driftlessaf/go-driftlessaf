/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stateTestBotID is the viewer the mock server reports, and so the creator
// Load trusts for state attachments.
const stateTestBotID = "bot-1"

// testBotState is a sample bot domain struct used in tests.
type testBotState struct {
	Location string `json:"location"`
	Health   int    `json:"health"`
}

// newStateTestServer creates a mock server that serves attachment content
// and handles GraphQL mutations (comment create/update, file upload, attachment ops).
func newStateTestServer(t *testing.T, attachmentContent map[string][]byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handle attachment content downloads (GET requests).
		if r.Method == http.MethodGet {
			for path, content := range attachmentContent {
				if strings.HasSuffix(r.URL.Path, path) {
					w.Header().Set("Content-Type", "application/json")
					w.Write(content)
					return
				}
			}
			http.NotFound(w, r)
			return
		}

		// Handle PUT for file upload.
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Handle GraphQL mutations.
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
		case strings.Contains(req.Query, "viewer"):
			resp = map[string]any{
				"viewer": map[string]any{"id": stateTestBotID, "name": "Test Bot"},
			}
		case strings.Contains(req.Query, "commentCreate"):
			resp = map[string]any{
				"commentCreate": map[string]any{
					"success": true,
					"comment": map[string]any{"id": "new-comment-1"},
				},
			}
		case strings.Contains(req.Query, "commentUpdate"):
			resp = map[string]any{
				"commentUpdate": map[string]any{"success": true},
			}
		case strings.Contains(req.Query, "fileUpload"):
			resp = map[string]any{
				"fileUpload": map[string]any{
					"uploadFile": map[string]any{
						"uploadUrl": "http://" + r.Host + "/upload",
						"assetUrl":  "https://uploads.linear.app/test/asset",
						"headers":   []any{},
					},
				},
			}
		case strings.Contains(req.Query, "attachmentDelete"):
			resp = map[string]any{
				"attachmentDelete": map[string]any{"success": true},
			}
		case strings.Contains(req.Query, "attachmentCreate"):
			resp = map[string]any{
				"attachmentCreate": map[string]any{"success": true},
			}
		default:
			resp = map[string]any{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": resp})
	}))
}

func TestStateManager_LoadSave_RoundTrip(t *testing.T) {
	// State JSON with metadata injected.
	stateJSON := `{"location":"Engine Room","health":80,"_commentId":"comment-42","_lastProcessedCommentId":"lpc-1"}`

	srv := newStateTestServer(t, map[string][]byte{
		"/state.json": []byte(stateJSON),
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)
	client.statePrefix = "test"

	issue := &Issue{ID: "issue-1"}
	issue.Attachments.Nodes = []Attachment{
		{ID: "att-1", Title: "test_state", URL: srv.URL + "/state.json", Creator: &User{ID: stateTestBotID}},
	}

	sm := client.NewStateManager(issue)

	var state testBotState
	found, err := sm.Load(context.Background(), &state)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !found {
		t.Fatal("expected state to be found")
	}

	// Bot data should be populated.
	if state.Location != "Engine Room" {
		t.Errorf("Location = %q, want %q", state.Location, "Engine Room")
	}
	if state.Health != 80 {
		t.Errorf("Health = %d, want %d", state.Health, 80)
	}

	// Metadata should be extracted into StateManager.
	if sm.commentID != "comment-42" {
		t.Errorf("commentID = %q, want %q", sm.commentID, "comment-42")
	}
	if sm.lastProcessedCommentID != "lpc-1" {
		t.Errorf("lastProcessedCommentID = %q, want %q", sm.lastProcessedCommentID, "lpc-1")
	}
}

func TestStateManager_BotStructStaysClean(t *testing.T) {
	stateJSON := `{"location":"Bridge","health":100,"_commentId":"c-1","_lastProcessedCommentId":"lpc-1","extra_field":"ignored"}`

	srv := newStateTestServer(t, map[string][]byte{
		"/state.json": []byte(stateJSON),
	})
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)
	client.statePrefix = "test"

	issue := &Issue{ID: "issue-1"}
	issue.Attachments.Nodes = []Attachment{
		{ID: "att-1", Title: "test_state", URL: srv.URL + "/state.json", Creator: &User{ID: stateTestBotID}},
	}

	sm := client.NewStateManager(issue)

	// Unmarshal into a map to verify no metadata keys leak through.
	var raw map[string]json.RawMessage
	found, err := sm.Load(context.Background(), &raw)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !found {
		t.Fatal("expected state to be found")
	}

	if _, ok := raw["_commentId"]; ok {
		t.Error("_commentId should not be in bot state")
	}
	if _, ok := raw["_lastProcessedCommentId"]; ok {
		t.Error("_lastProcessedCommentId should not be in bot state")
	}
	// Non-metadata fields should survive.
	if _, ok := raw["location"]; !ok {
		t.Error("location should be in bot state")
	}
}

func TestStateManager_LoadNoAttachment(t *testing.T) {
	client := NewClientWithAPIKey("test-token")
	client.statePrefix = "test"

	issue := &Issue{ID: "issue-1"}
	// No attachments.

	sm := client.NewStateManager(issue)

	var state testBotState
	found, err := sm.Load(context.Background(), &state)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if found {
		t.Error("expected state not found for empty attachments")
	}
}

func TestStateManager_UnprocessedComments_LoopPrevention(t *testing.T) {
	botID := "bot-1"

	issue := &Issue{ID: "issue-1"}
	issue.Comments.Nodes = []Comment{
		{ID: "c1", Body: "bot response", User: User{ID: botID}, CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "c2", Body: "player move", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-1 * time.Minute)},
	}

	client := NewClientWithAPIKey("test-token")
	sm := client.NewStateManager(issue)

	// First call: should return the unprocessed comment.
	unprocessed := sm.UnprocessedComments(botID)
	if len(unprocessed) != 1 {
		t.Fatalf("expected 1 unprocessed comment, got %d", len(unprocessed))
	}
	if unprocessed[0].ID != "c2" {
		t.Errorf("unprocessed[0].ID = %q, want %q", unprocessed[0].ID, "c2")
	}

	// Simulate what Save does: mark pending comments as processed.
	sm.lastProcessedCommentID = sm.pendingComments[len(sm.pendingComments)-1].ID
	sm.pendingComments = nil

	// Second call (simulating re-reconciliation): should return nil.
	unprocessed = sm.UnprocessedComments(botID)
	if len(unprocessed) != 0 {
		t.Errorf("expected 0 unprocessed comments after marking processed, got %d", len(unprocessed))
	}
}

func TestStateManager_UnprocessedComments_NewCommentBreaksLoop(t *testing.T) {
	botID := "bot-1"

	issue := &Issue{ID: "issue-1"}
	issue.Comments.Nodes = []Comment{
		{ID: "c1", Body: "bot response", User: User{ID: botID}, CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "c2", Body: "player move", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-1 * time.Minute)},
	}

	client := NewClientWithAPIKey("test-token")
	sm := client.NewStateManager(issue)
	sm.lastProcessedCommentID = "c2" // Already processed c2.

	// Should return nil since c2 is already processed.
	unprocessed := sm.UnprocessedComments(botID)
	if len(unprocessed) != 0 {
		t.Fatalf("expected 0 unprocessed comments, got %d", len(unprocessed))
	}

	// Now a new comment arrives.
	issue.Comments.Nodes = append(issue.Comments.Nodes, Comment{
		ID: "c3", Body: "another move", User: User{ID: "player-1"}, CreatedAt: time.Now(),
	})

	// Should return the new comment.
	unprocessed = sm.UnprocessedComments(botID)
	if len(unprocessed) != 2 {
		t.Fatalf("expected 2 unprocessed comments, got %d", len(unprocessed))
	}
	if unprocessed[1].ID != "c3" {
		t.Errorf("unprocessed[1].ID = %q, want %q", unprocessed[1].ID, "c3")
	}
}

func TestStateManager_SaveMarksProcessed(t *testing.T) {
	botID := "bot-1"

	srv := newStateTestServer(t, nil)
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)
	client.statePrefix = "test"

	issue := &Issue{ID: "issue-1"}
	issue.Comments.Nodes = []Comment{
		{ID: "c1", Body: "bot response", User: User{ID: botID}, CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "c2", Body: "player move", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-1 * time.Minute)},
	}

	sm := client.NewStateManager(issue)

	// Get unprocessed comments — sets pendingComments.
	unprocessed := sm.UnprocessedComments(botID)
	if len(unprocessed) != 1 {
		t.Fatalf("expected 1 unprocessed comment, got %d", len(unprocessed))
	}

	// Save should mark them as processed.
	state := testBotState{Location: "Bridge", Health: 100}
	if err := sm.Save(context.Background(), &state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if sm.lastProcessedCommentID != "c2" {
		t.Errorf("lastProcessedCommentID = %q, want %q", sm.lastProcessedCommentID, "c2")
	}
	if sm.pendingComments != nil {
		t.Error("pendingComments should be nil after Save")
	}
}

func TestStateManager_UpsertBotComment_TracksID(t *testing.T) {
	srv := newStateTestServer(t, nil)
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)

	issue := &Issue{ID: "issue-1"}
	sm := client.NewStateManager(issue)

	// First call should create a new comment.
	if err := sm.UpsertBotComment(context.Background(), "hello"); err != nil {
		t.Fatalf("UpsertBotComment failed: %v", err)
	}
	if sm.commentID != "new-comment-1" {
		t.Errorf("commentID = %q, want %q", sm.commentID, "new-comment-1")
	}

	// Second call should update (commentID is now set).
	if err := sm.UpsertBotComment(context.Background(), "updated"); err != nil {
		t.Fatalf("UpsertBotComment update failed: %v", err)
	}
	// ID should remain the same.
	if sm.commentID != "new-comment-1" {
		t.Errorf("commentID = %q, want %q", sm.commentID, "new-comment-1")
	}
}

func TestStateManager_SaveInjectsMetadata(t *testing.T) {
	// Capture what gets uploaded.
	var uploadedData []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			uploadedData, _ = json.Marshal(json.RawMessage(readBody(r)))
			w.WriteHeader(http.StatusOK)
			return
		}

		var req struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		var resp any
		switch {
		case strings.Contains(req.Query, "fileUpload"):
			resp = map[string]any{
				"fileUpload": map[string]any{
					"uploadFile": map[string]any{
						"uploadUrl": "http://" + r.Host + "/upload",
						"assetUrl":  "https://uploads.linear.app/test/asset",
						"headers":   []any{},
					},
				},
			}
		case strings.Contains(req.Query, "attachmentCreate"):
			resp = map[string]any{
				"attachmentCreate": map[string]any{"success": true},
			}
		default:
			resp = map[string]any{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": resp})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-token").
		WithHTTPClient(srv.Client()).
		WithEndpoint(srv.URL)
	client.statePrefix = "test"

	issue := &Issue{ID: "issue-1"}
	sm := client.NewStateManager(issue)
	sm.commentID = "comment-42"
	sm.lastProcessedCommentID = "lpc-99"

	state := testBotState{Location: "Bridge", Health: 100}
	if err := sm.Save(context.Background(), &state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Parse what was uploaded and verify metadata was injected.
	var uploaded map[string]json.RawMessage
	if err := json.Unmarshal(uploadedData, &uploaded); err != nil {
		t.Fatalf("failed to parse uploaded data: %v", err)
	}

	var commentID string
	if err := json.Unmarshal(uploaded[metaKeyCommentID], &commentID); err != nil {
		t.Fatalf("missing _commentId in uploaded data: %v", err)
	}
	if commentID != "comment-42" {
		t.Errorf("_commentId = %q, want %q", commentID, "comment-42")
	}

	var lastProcessed string
	if err := json.Unmarshal(uploaded[metaKeyLastProcessedCommentID], &lastProcessed); err != nil {
		t.Fatalf("missing _lastProcessedCommentId in uploaded data: %v", err)
	}
	if lastProcessed != "lpc-99" {
		t.Errorf("_lastProcessedCommentId = %q, want %q", lastProcessed, "lpc-99")
	}

	// Bot data should also be present.
	var location string
	if err := json.Unmarshal(uploaded["location"], &location); err != nil {
		t.Fatalf("missing location in uploaded data: %v", err)
	}
	if location != "Bridge" {
		t.Errorf("location = %q, want %q", location, "Bridge")
	}
}

func readBody(r *http.Request) []byte {
	defer r.Body.Close()
	data, _ := json.Marshal(json.RawMessage(mustReadAll(r.Body)))
	return data
}

func mustReadAll(r interface{ Read([]byte) (int, error) }) []byte {
	var buf []byte
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}
