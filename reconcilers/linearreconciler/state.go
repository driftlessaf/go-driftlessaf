/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chainguard-dev/clog"
)

const (
	metaKeyCommentID              = "_commentId"
	metaKeyLastProcessedCommentID = "_lastProcessedCommentId"
)

// StateManager manages bot state and internal metadata for a single issue.
// It transparently stores package metadata (bot comment ID, last processed
// comment ID) alongside the bot's domain state in a single file attachment.
//
// Usage:
//
//	sm := client.NewStateManager(issue)
//	var state MyState
//	sm.Load(ctx, &state)
//	if comments := sm.UnprocessedComments(botUserID); len(comments) > 0 {
//	    // ... process comments ...
//	    sm.UpsertBotComment(ctx, "response")
//	}
//	sm.Save(ctx, &state)
type StateManager struct {
	client                 *Client
	issue                  *Issue
	commentID              string
	lastProcessedCommentID string
	pendingComments        []Comment // set by UnprocessedComments, consumed by Save
}

// NewStateManager creates a StateManager for the given issue.
func (c *Client) NewStateManager(issue *Issue) *StateManager {
	return &StateManager{
		client: c,
		issue:  issue,
	}
}

// stateAttachmentTitle returns the attachment title used for state storage.
func (c *Client) stateAttachmentTitle() string {
	return c.statePrefix + "_state"
}

// UnprocessedComments returns player comments that haven't been processed yet.
// It combines issue.UnprocessedComments (comments after the last bot comment)
// with the StateManager's tracking of previously processed comments to prevent
// re-processing when attachment updates trigger re-reconciliation.
func (sm *StateManager) UnprocessedComments(botUserID string) []Comment {
	unprocessed := sm.issue.UnprocessedComments(botUserID)
	if len(unprocessed) == 0 {
		return nil
	}

	// If the last unprocessed comment was already processed, skip all.
	lastID := unprocessed[len(unprocessed)-1].ID
	if lastID == sm.lastProcessedCommentID {
		return nil
	}

	sm.pendingComments = unprocessed
	return unprocessed
}

// Load reads the state attachment and deserializes the bot data into v.
// Internal metadata is extracted and stored on the StateManager.
// Returns true if state was found and loaded, false if no state exists.
func (sm *StateManager) Load(ctx context.Context, v any) (bool, error) {
	att := sm.issue.FindAttachment(sm.client.stateAttachmentTitle())
	if att == nil || att.URL == "" {
		return false, nil
	}

	data, err := sm.client.FetchAttachmentContent(ctx, att.URL)
	if err != nil {
		return false, fmt.Errorf("fetching state attachment: %w", err)
	}

	// Extract internal metadata before unmarshaling into the bot's struct.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("unmarshaling state envelope: %w", err)
	}

	if cid, ok := raw[metaKeyCommentID]; ok {
		var commentID string
		if err := json.Unmarshal(cid, &commentID); err == nil {
			sm.commentID = commentID
		}
		delete(raw, metaKeyCommentID)
	}

	if lcid, ok := raw[metaKeyLastProcessedCommentID]; ok {
		var lastProcessed string
		if err := json.Unmarshal(lcid, &lastProcessed); err == nil {
			sm.lastProcessedCommentID = lastProcessed
		}
		delete(raw, metaKeyLastProcessedCommentID)
	}

	// Re-marshal without metadata keys and unmarshal into the bot's struct.
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return false, fmt.Errorf("re-marshaling state: %w", err)
	}
	if err := json.Unmarshal(cleaned, v); err != nil {
		return false, fmt.Errorf("unmarshaling bot state: %w", err)
	}

	clog.InfoContextf(ctx, "Loaded state from attachment %q", sm.client.stateAttachmentTitle())
	return true, nil
}

// Save serializes the bot data from v, merges internal metadata, and uploads
// as a file attachment. Any existing state attachments are deleted before uploading
// to prevent orphans. Automatically marks any pending comments (from
// UnprocessedComments) as processed.
func (sm *StateManager) Save(ctx context.Context, v any) error {
	// Mark pending comments as processed.
	if len(sm.pendingComments) > 0 {
		sm.lastProcessedCommentID = sm.pendingComments[len(sm.pendingComments)-1].ID
		sm.pendingComments = nil
	}

	// Marshal the bot's struct to a map so we can inject metadata.
	botData, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling bot state: %w", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(botData, &merged); err != nil {
		return fmt.Errorf("unmarshaling bot state for merge: %w", err)
	}

	// Inject internal metadata.
	if sm.commentID != "" {
		cid, _ := json.Marshal(sm.commentID)
		merged[metaKeyCommentID] = cid
	}
	if sm.lastProcessedCommentID != "" {
		lcid, _ := json.Marshal(sm.lastProcessedCommentID)
		merged[metaKeyLastProcessedCommentID] = lcid
	}

	data, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshaling merged state: %w", err)
	}

	log := clog.FromContext(ctx)
	title := sm.client.stateAttachmentTitle()

	// Delete all existing state attachments to prevent orphans.
	for _, att := range sm.issue.Attachments.Nodes {
		if att.Title != title {
			continue
		}
		if err := sm.client.DeleteAttachment(ctx, att.ID); err != nil {
			log.With("attachment_id", att.ID).Warnf("Failed to delete old state attachment: %v", err)
		}
	}

	if err := sm.client.UploadFileAttachment(ctx, sm.issue.ID, title, data); err != nil {
		return fmt.Errorf("uploading state attachment: %w", err)
	}

	log.Infof("Saved state to attachment %q", title)
	return nil
}

// UpsertBotComment creates or updates the bot's comment on the issue.
// The comment ID is tracked internally and persisted on the next Save call.
func (sm *StateManager) UpsertBotComment(ctx context.Context, body string) error {
	commentID, err := sm.client.upsertComment(ctx, sm.issue.ID, sm.commentID, body)
	if err != nil {
		return fmt.Errorf("upserting bot comment: %w", err)
	}
	sm.commentID = commentID
	return nil
}

// LoadState fetches the state attachment from the issue, deserializes it into v.
// For simple use cases that don't need comment tracking. Use NewStateManager
// for full functionality.
func (c *Client) LoadState(ctx context.Context, issue *Issue, v any) (bool, error) {
	sm := c.NewStateManager(issue)
	return sm.Load(ctx, v)
}

// SaveState serializes v as JSON and uploads it as a file attachment on the issue.
// For simple use cases that don't need comment tracking. Use NewStateManager
// for full functionality.
func (c *Client) SaveState(ctx context.Context, issue *Issue, v any) error {
	sm := c.NewStateManager(issue)
	return sm.Save(ctx, v)
}
