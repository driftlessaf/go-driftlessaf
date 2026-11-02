/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentack

import (
	"cmp"
	"slices"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/commentaudience"
)

// botType is the GraphQL __typename GitHub reports for a GitHub App account.
const botType = "Bot"

// Comment holds what the ack decision needs to know about one comment.
type Comment struct {
	NodeID string
	// AuthorType is the GraphQL __typename of the author: "User", "Bot",
	// "Mannequin" or "Organization". It is empty when the account was deleted.
	AuthorType        string
	AuthorAssociation string
	Body              string
	CreatedAt         time.Time
	IsMinimized       bool
	// EyesFromBot is true when any account that reacted with 👀 is a bot.
	EyesFromBot bool
	// Inline is true for a comment in a review thread, false for a comment in
	// the pull request conversation.
	Inline bool
}

// Options controls which comments Pending selects.
type Options struct {
	Now time.Time
	// Window is how far back a comment can have been created and still be acked.
	Window time.Duration
	// Since is the date of the pull request's head commit. A conversation
	// comment created at or before it is not acked, since the bot does not act
	// on it. Inline comments are not cut off: the bot acts on an open thread
	// whatever its age. The zero time means no cutoff. Ack sets it from what
	// API.List returns.
	Since time.Time
	// Trusted reports whether a comment author's association is one the bot
	// acts on.
	Trusted func(association string) bool
	// BotLogin is the bot's login. A comment that mentions anyone else is not
	// meant for the bot, so it is skipped. When BotLogin is empty, a comment
	// with any mention is skipped.
	BotLogin string
}

// Pending returns the node IDs of the comments that still need a 👀, oldest
// first. A comment needs one when a person with a trusted association wrote
// it, it was created within the window and, for a conversation comment, after Since, it is not minimized,
// it does not mention someone other than the bot, and no bot has reacted with
// 👀 yet.
//
// Any bot counts, not only the caller's own account: the app behind a bot can
// run as several accounts, so "my own reaction" can miss a 👀 an earlier run
// left under another account.
func Pending(comments []Comment, o Options) []string {
	pending := make([]Comment, 0, len(comments))
	for _, c := range comments {
		if c.AuthorType == "" || c.AuthorType == botType || c.IsMinimized || c.EyesFromBot {
			continue
		}
		if !o.Trusted(c.AuthorAssociation) || o.Now.Sub(c.CreatedAt) > o.Window {
			continue
		}
		if !c.Inline && !o.Since.IsZero() && !c.CreatedAt.After(o.Since) {
			continue
		}
		if commentaudience.ForSomeoneElse(c.Body, o.BotLogin) {
			continue
		}
		pending = append(pending, c)
	}
	slices.SortStableFunc(pending, func(a, b Comment) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.NodeID, b.NodeID))
	})
	ids := make([]string, 0, len(pending))
	for _, c := range pending {
		ids = append(ids, c.NodeID)
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}
