/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"strings"
	"time"
)

// User represents a Linear user.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// App is true for organization app users (e.g. incident.io, GitHub).
	// App users generally cannot be assigned issues unless their app
	// carries the assignable scope — issueCreate with such an assigneeId
	// fails with "App user not valid".
	App bool `json:"app"`
}

// Comment represents a comment on a Linear issue.
type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	User      User      `json:"user"`
}

// Attachment represents a file or link attachment on a Linear issue.
type Attachment struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Subtitle  string    `json:"subtitle"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
}

// Issue represents a Linear issue with its comments, attachments, and labels.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// URL is the canonical Linear-hosted URL for the issue (e.g.
	// "https://linear.app/{workspace}/issue/{identifier}"). Populated by
	// GetIssue.
	URL       string `json:"url"`
	UpdatedAt string `json:"updatedAt"`

	State struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`

	Team struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`

	Assignee *User `json:"assignee"`

	// Creator is the Linear user who originally filed the issue. Populated
	// by GetIssue. Nil for issues created via integrations (e.g. webhook
	// imports) that don't carry a Linear-user attribution.
	Creator *User `json:"creator"`

	Attachments struct {
		Nodes []Attachment `json:"nodes"`
	} `json:"attachments"`

	Comments struct {
		Nodes []Comment `json:"nodes"`
	} `json:"comments"`

	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`

	// Documents are Linear documents whose parent is this issue (via
	// `Document.issue`). Native first-class link surfaced in Linear's
	// right-rail "Documents" panel — distinct from URL attachments in
	// `Attachments.Nodes`. Fetched by GetIssue with the same id/slugId/url
	// fragment GetDocument uses.
	Documents struct {
		Nodes []struct {
			ID    string `json:"id"`
			Slug  string `json:"slugId"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"nodes"`
	} `json:"documents"`
}

// HasLabel returns true if the issue has a label with the given name (case-insensitive).
func (i *Issue) HasLabel(name string) bool {
	for _, l := range i.Labels.Nodes {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// LabelNames returns the names of labels attached to this issue, in the order
// Linear returned them. Names are returned verbatim (no case folding) — use
// HasLabel for case-insensitive comparisons.
func (i *Issue) LabelNames() []string {
	names := make([]string, 0, len(i.Labels.Nodes))
	for _, l := range i.Labels.Nodes {
		names = append(names, l.Name)
	}
	return names
}

// FindAttachment returns the first attachment matching the given title, or nil.
func (i *Issue) FindAttachment(title string) *Attachment {
	for idx := range i.Attachments.Nodes {
		if i.Attachments.Nodes[idx].Title == title {
			return &i.Attachments.Nodes[idx]
		}
	}
	return nil
}

// FindAttachmentsByTitle returns every attachment whose title matches, in
// the order Linear returned them. Useful when a delete-then-upload upsert
// has raced and left multiple attachments under the same title — callers
// can merge their payloads instead of silently picking whichever happens
// to come first.
func (i *Issue) FindAttachmentsByTitle(title string) []Attachment {
	var matches []Attachment
	for idx := range i.Attachments.Nodes {
		if i.Attachments.Nodes[idx].Title == title {
			matches = append(matches, i.Attachments.Nodes[idx])
		}
	}
	return matches
}

// FindLatestAttachment returns the matching attachment with the most recent
// CreatedAt, or nil if no match exists. Use this instead of FindAttachment
// when reading state attachments written by a delete-then-upload Save() —
// concurrent reconciliations can race the delete and leave multiple
// attachments under the same title, and the latest is the source of truth.
// Falls back to the first match when CreatedAt is zero (e.g. older queries
// that didn't request the field).
func (i *Issue) FindLatestAttachment(title string) *Attachment {
	var latest *Attachment
	for idx := range i.Attachments.Nodes {
		if i.Attachments.Nodes[idx].Title != title {
			continue
		}
		a := &i.Attachments.Nodes[idx]
		if latest == nil || a.CreatedAt.After(latest.CreatedAt) {
			latest = a
		}
	}
	return latest
}

// UnprocessedComments returns comments that appear after the last comment
// by the given botUserID. If there are no bot comments, all comments are
// returned. Returns nil if the last comment is from the bot.
func (i *Issue) UnprocessedComments(botUserID string) []Comment {
	lastBotIdx := -1
	for idx, c := range i.Comments.Nodes {
		if c.User.ID == botUserID {
			lastBotIdx = idx
		}
	}

	// All comments are unprocessed if the bot has never commented.
	if lastBotIdx == -1 {
		return i.Comments.Nodes
	}

	// Nothing to process if the bot's comment is the last one.
	remaining := i.Comments.Nodes[lastBotIdx+1:]
	if len(remaining) == 0 {
		return nil
	}

	return remaining
}

// HasUnprocessedComments returns true if there are comments after the last
// comment by the given botUserID.
func (i *Issue) HasUnprocessedComments(botUserID string) bool {
	return len(i.UnprocessedComments(botUserID)) > 0
}
