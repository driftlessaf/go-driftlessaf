/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
)

// IssueThread is one read of the source issue's comments and timeline: this
// bot's notes under one marker, and the activity a reconciler can count as a
// request for another attempt. Session.ReadIssueThread produces it.
//
// "This bot" is the identity GraphQL's viewerDidAuthor reports, the one check
// that works for an App installation token (REST has no "who am I" for those).
// A bot whose tokens rotate across several App identities, such as Octo STS
// shards, would not recognise the notes it left under another identity.
type IssueThread struct {
	// LatestReplyID is the GraphQL node ID of the newest comment written by a
	// person, or "" when there is none. Comments by this bot or by any other
	// bot are left out, so that bots on one issue cannot prompt each other into
	// further runs.
	LatestReplyID string
	// LabeledAt holds, for each label applied among the issue's latest hundred
	// label and reopen events, when it was last applied.
	LabeledAt map[string]time.Time
	// ReopenedAt is when the issue was last reopened among those events, or
	// the zero time.
	ReopenedAt time.Time

	marker string
	notes  []issueNote
}

// issueNote is a comment this bot wrote whose body begins with the marker of
// the IssueThread holding it.
type issueNote struct {
	id   string // GraphQL node ID
	body string
}

// HasNote reports whether this bot left a note under the thread's marker
// stamped with revision (see Session.UpsertIssueNote). A nil thread has none.
func (t *IssueThread) HasNote(revision string) bool {
	return t != nil && slices.ContainsFunc(t.notes, func(n issueNote) bool {
		return hasRevision(n.body, t.marker, revision)
	})
}

// issueTimelineEvents bounds how many of the issue's most recent label and
// reopen events ReadIssueThread reads. A request for another attempt is
// recent by nature; an older event that falls outside the window reads as
// absent on every pass, so the thread stays stable rather than flapping.
const issueTimelineEvents = 100

// issueThreadQuery reads one page of the source issue's comments, oldest
// first, along with its latest label and reopen events. The events ride on
// every page so one query shape serves; ReadIssueThread uses the first
// page's.
type issueThreadQuery struct {
	Repository struct {
		Issue *struct {
			Comments struct {
				Nodes []struct {
					ID              string
					Body            string
					ViewerDidAuthor bool
					Author          *struct {
						Typename string `graphql:"__typename"`
					}
				}
				PageInfo struct {
					HasNextPage bool
					EndCursor   githubv4.String
				}
			} `graphql:"comments(first: 100, after: $after)"`
			TimelineItems struct {
				Nodes []struct {
					Typename     string `graphql:"__typename"`
					LabeledEvent struct {
						CreatedAt githubv4.DateTime
						Label     struct{ Name string }
					} `graphql:"... on LabeledEvent"`
					ReopenedEvent struct {
						CreatedAt githubv4.DateTime
					} `graphql:"... on ReopenedEvent"`
				}
			} `graphql:"timelineItems(last: $events, itemTypes: [LABELED_EVENT, REOPENED_EVENT])"`
		} `graphql:"issue(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $repo)"`
}

// ReadIssueThread reads the source issue's comments, paging through all of
// them so that a note anywhere in a long thread is found, and its latest label
// and reopen events. Notes are this bot's comments whose bodies begin with
// marker; a comment by anyone else that copies the marker is neither a note
// nor a reply. It returns an empty thread on a non-issue resource.
func (s *Session[T]) ReadIssueThread(ctx context.Context, marker string) (*IssueThread, error) {
	thread := &IssueThread{marker: marker, LabeledAt: map[string]time.Time{}}
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return thread, nil
	}
	if marker == "" {
		return nil, errors.New("empty marker would match every comment")
	}
	if s.gqlClient == nil {
		return nil, errors.New("no GraphQL client on session")
	}
	vars := map[string]any{
		"owner":  githubv4.String(s.resource.Owner),
		"repo":   githubv4.String(s.resource.Repo),
		"number": githubv4.Int(s.resource.Number),
		"events": githubv4.Int(issueTimelineEvents),
		"after":  (*githubv4.String)(nil),
	}
	for first := true; ; first = false {
		var q issueThreadQuery
		if err := s.gqlClient.Query(ctx, "IssueThread", &q, vars); err != nil {
			return nil, fmt.Errorf("reading issue thread: %w", err)
		}
		issue := q.Repository.Issue
		if issue == nil {
			return nil, fmt.Errorf("reading issue thread: issue #%d not found", s.resource.Number)
		}
		if first {
			for _, e := range issue.TimelineItems.Nodes {
				switch e.Typename {
				case "LabeledEvent":
					name := e.LabeledEvent.Label.Name
					thread.LabeledAt[name] = laterOf(thread.LabeledAt[name], e.LabeledEvent.CreatedAt.Time)
				case "ReopenedEvent":
					thread.ReopenedAt = laterOf(thread.ReopenedAt, e.ReopenedEvent.CreatedAt.Time)
				}
			}
		}
		for _, c := range issue.Comments.Nodes {
			switch {
			case c.ViewerDidAuthor:
				if strings.HasPrefix(c.Body, marker) {
					thread.notes = append(thread.notes, issueNote{id: c.ID, body: c.Body})
				}
			case c.Author == nil || c.Author.Typename != "Bot":
				// A nil author is a deleted account, which was a person's.
				thread.LatestReplyID = c.ID
			}
		}
		if !issue.Comments.PageInfo.HasNextPage {
			return thread, nil
		}
		vars["after"] = new(issue.Comments.PageInfo.EndCursor)
	}
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// UpsertIssueNote leaves body on the source issue as this bot's one note under
// marker for revision. A note already stamped with revision is kept even when
// body differs, since body (an agent's prose, say) varies run to run while
// what it answers does not. Otherwise body is posted as a fresh comment, so
// whoever prompted the run is notified of the answer. Every other note this
// bot left under marker is deleted only after that post succeeds, so a failed
// post leaves the earlier note in place rather than none. A failed delete is
// logged and leaves a duplicate, which the next UpsertIssueNote or
// DeleteIssueNotes removes. A comment by anyone else that copies the marker is
// never deleted: the bot's token may delete any comment on the issue.
//
// It reports whether a note was posted (false on a kept note, a tolerated
// permission error, or a non-issue resource).
func (s *Session[T]) UpsertIssueNote(ctx context.Context, marker, revision, body string) (bool, error) {
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return false, nil
	}
	thread, err := s.ReadIssueThread(ctx, marker)
	if err != nil {
		return false, err
	}
	keep := slices.IndexFunc(thread.notes, func(n issueNote) bool { return hasRevision(n.body, marker, revision) })
	posted := false
	if keep < 0 {
		number := s.resource.Number
		clog.InfoContextf(ctx, "Posting note on #%d", number)
		if _, _, err := s.client.Issues.CreateComment(ctx, s.resource.Owner, s.resource.Repo, number, &github.IssueComment{
			Body: new(revisionNote(marker, revision, body)),
		}); err != nil {
			_, err = s.skipMarkerCommentIfForbidden(ctx, number, "posting note", err)
			return false, err
		}
		posted = true
	}
	if err := s.deleteIssueNotes(ctx, thread.notes, keep); err != nil {
		clog.WarnContext(ctx, "Leaving a duplicate note", "number", s.resource.Number, "error", err)
	}
	return posted, nil
}

// DeleteIssueNotes deletes every note this bot left on the source issue under
// marker, leaving alone any comment by anyone else that copies the marker. It
// is a no-op on non-issue resources or when there are no notes.
func (s *Session[T]) DeleteIssueNotes(ctx context.Context, marker string) error {
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return nil
	}
	thread, err := s.ReadIssueThread(ctx, marker)
	if err != nil {
		return err
	}
	return s.deleteIssueNotes(ctx, thread.notes, -1)
}

// deleteIssueNotes deletes notes other than the one at index keep (-1 for
// none), attempting each and returning every failure.
func (s *Session[T]) deleteIssueNotes(ctx context.Context, notes []issueNote, keep int) error {
	var errs []error
	for i, n := range notes {
		if i == keep {
			continue
		}
		clog.InfoContextf(ctx, "Deleting note on #%d", s.resource.Number)
		var mutation struct {
			DeleteIssueComment struct {
				ClientMutationId string
			} `graphql:"deleteIssueComment(input: $input)"`
		}
		if err := s.gqlClient.Mutate(ctx, "DeleteIssueComment", &mutation, githubv4.DeleteIssueCommentInput{
			ID: githubv4.ID(n.id),
		}, nil); err != nil {
			errs = append(errs, fmt.Errorf("deleting note %s: %w", n.id, err))
		}
	}
	return errors.Join(errs...)
}

// revisionMarker renders the hidden line that stamps a marker comment with the
// revision of its subject.
func revisionMarker(revision string) string {
	return "<!--revision:" + revision + "-->"
}

// revisionNote is the body of a marker comment stamped with revision: the
// marker, the revision line, then body.
func revisionNote(marker, revision, body string) string {
	return marker + "\n" + revisionMarker(revision) + "\n" + body
}

// hasRevision reports whether body is a marker comment stamped with revision,
// laid out as revisionNote writes it. It accepts CRLF line endings, which
// GitHub stores when someone edits a comment in the web UI.
func hasRevision(body, marker, revision string) bool {
	rest, ok := strings.CutPrefix(body, marker)
	if !ok {
		return false
	}
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, "\r"), "\n")
	line, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSuffix(line, "\r") == revisionMarker(revision)
}
