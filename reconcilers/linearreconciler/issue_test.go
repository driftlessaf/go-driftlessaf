/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"testing"
	"time"
)

func TestIssue_TeamID(t *testing.T) {
	issue := &Issue{}
	issue.Team.ID = "team-uuid-abc"
	issue.Team.Key = "ENG"
	issue.Team.Name = "Engineering"

	if issue.Team.ID != "team-uuid-abc" {
		t.Errorf("Team.ID = %q, want team-uuid-abc", issue.Team.ID)
	}
	if issue.Team.Key != "ENG" {
		t.Errorf("Team.Key = %q, want ENG", issue.Team.Key)
	}
}

func TestIssue_HasLabel(t *testing.T) {
	issue := &Issue{}
	issue.Labels.Nodes = []struct {
		Name string `json:"name"`
	}{
		{Name: "game"},
		{Name: "Bug"},
	}

	if !issue.HasLabel("game") {
		t.Error("expected HasLabel(game) = true")
	}
	if !issue.HasLabel("GAME") {
		t.Error("expected HasLabel(GAME) = true (case-insensitive)")
	}
	if !issue.HasLabel("bug") {
		t.Error("expected HasLabel(bug) = true (case-insensitive)")
	}
	if issue.HasLabel("feature") {
		t.Error("expected HasLabel(feature) = false")
	}
}

func TestIssue_FindAttachment(t *testing.T) {
	issue := &Issue{}
	issue.Attachments.Nodes = []Attachment{
		{ID: "a1", Title: "game_state", URL: "https://example.com/state.json"},
		{ID: "a2", Title: "other", URL: "https://example.com/other.json"},
	}

	att := issue.FindAttachment("game_state")
	if att == nil {
		t.Fatal("expected to find game_state attachment")
	}
	if att.ID != "a1" {
		t.Errorf("got ID %s, want a1", att.ID)
	}

	if issue.FindAttachment("missing") != nil {
		t.Error("expected FindAttachment(missing) = nil")
	}
}

func TestIssue_FindLatestAttachment(t *testing.T) {
	t0 := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	t2 := t0.Add(2 * time.Minute)

	t.Run("picks latest among duplicates", func(t *testing.T) {
		issue := &Issue{}
		issue.Attachments.Nodes = []Attachment{
			{ID: "a1", Title: "state", URL: "u1", CreatedAt: t0},
			{ID: "other", Title: "design_doc", URL: "u-doc", CreatedAt: t2},
			{ID: "a2", Title: "state", URL: "u2", CreatedAt: t2},
			{ID: "a3", Title: "state", URL: "u3", CreatedAt: t1},
		}
		got := issue.FindLatestAttachment("state")
		if got == nil {
			t.Fatal("expected match")
		}
		if got.ID != "a2" {
			t.Errorf("got ID %s, want a2 (latest CreatedAt)", got.ID)
		}
	})

	t.Run("returns nil when no match", func(t *testing.T) {
		issue := &Issue{}
		issue.Attachments.Nodes = []Attachment{{ID: "x", Title: "other"}}
		if got := issue.FindLatestAttachment("missing"); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("falls back to first when CreatedAt is zero", func(t *testing.T) {
		issue := &Issue{}
		issue.Attachments.Nodes = []Attachment{
			{ID: "a1", Title: "state"},
			{ID: "a2", Title: "state"},
		}
		got := issue.FindLatestAttachment("state")
		if got == nil || got.ID != "a1" {
			t.Errorf("got %+v, want a1 (first when timestamps tie)", got)
		}
	})
}

func TestIssue_FindAttachmentsByTitle(t *testing.T) {
	issue := &Issue{}
	issue.Attachments.Nodes = []Attachment{
		{ID: "a1", Title: "game_state", URL: "https://example.com/v1.json"},
		{ID: "a2", Title: "other", URL: "https://example.com/other.json"},
		{ID: "a3", Title: "game_state", URL: "https://example.com/v2.json"},
	}

	got := issue.FindAttachmentsByTitle("game_state")
	if len(got) != 2 {
		t.Fatalf("expected 2 game_state attachments, got %d", len(got))
	}
	if got[0].ID != "a1" || got[1].ID != "a3" {
		t.Errorf("got IDs %v, want [a1 a3] (preserves Linear's order)", []string{got[0].ID, got[1].ID})
	}

	if names := issue.FindAttachmentsByTitle("missing"); names != nil {
		t.Errorf("expected nil for missing title, got %v", names)
	}
}

func TestIssue_UnprocessedComments(t *testing.T) {
	botID := "bot-123"

	tests := []struct {
		name     string
		comments []Comment
		wantLen  int
	}{
		{
			name:     "no comments",
			comments: nil,
			wantLen:  0,
		},
		{
			name: "all player comments",
			comments: []Comment{
				{ID: "c1", Body: "hello", User: User{ID: "player-1"}},
				{ID: "c2", Body: "world", User: User{ID: "player-2"}},
			},
			wantLen: 2,
		},
		{
			name: "bot responded last",
			comments: []Comment{
				{ID: "c1", Body: "hello", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-2 * time.Minute)},
				{ID: "c2", Body: "response", User: User{ID: botID}, CreatedAt: time.Now().Add(-1 * time.Minute)},
			},
			wantLen: 0,
		},
		{
			name: "player after bot",
			comments: []Comment{
				{ID: "c1", Body: "hello", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-3 * time.Minute)},
				{ID: "c2", Body: "response", User: User{ID: botID}, CreatedAt: time.Now().Add(-2 * time.Minute)},
				{ID: "c3", Body: "next move", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-1 * time.Minute)},
			},
			wantLen: 1,
		},
		{
			name: "multiple players after bot",
			comments: []Comment{
				{ID: "c1", Body: "response", User: User{ID: botID}, CreatedAt: time.Now().Add(-3 * time.Minute)},
				{ID: "c2", Body: "move 1", User: User{ID: "player-1"}, CreatedAt: time.Now().Add(-2 * time.Minute)},
				{ID: "c3", Body: "move 2", User: User{ID: "player-2"}, CreatedAt: time.Now().Add(-1 * time.Minute)},
			},
			wantLen: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue := &Issue{}
			issue.Comments.Nodes = tc.comments

			got := issue.UnprocessedComments(botID)
			if len(got) != tc.wantLen {
				t.Errorf("UnprocessedComments() returned %d comments, want %d", len(got), tc.wantLen)
			}

			hasUnprocessed := issue.HasUnprocessedComments(botID)
			wantHas := tc.wantLen > 0
			if hasUnprocessed != wantHas {
				t.Errorf("HasUnprocessedComments() = %v, want %v", hasUnprocessed, wantHas)
			}
		})
	}
}
