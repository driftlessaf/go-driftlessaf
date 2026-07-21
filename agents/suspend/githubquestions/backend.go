/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package githubquestions

import (
	"context"
	"fmt"
	"slices"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
)

// maxCommentPages bounds how far back the store scans a conversation.
// Pages are fetched newest-first, so on a conversation larger than the bound
// the scan drops only the oldest history — the question marker and its
// answers are always near the tail of the conversation and stay inside the
// window.
const maxCommentPages = 10

// commentsBackend abstracts the GitHub issue-comment operations Store needs,
// so tests can substitute an in-memory fake.
type commentsBackend interface {
	// listComments returns the issue/PR comments in ascending creation order.
	listComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error)

	// createComment posts a new comment.
	createComment(ctx context.Context, owner, repo string, number int, body string) error

	// editComment replaces the body of an existing comment.
	editComment(ctx context.Context, owner, repo string, commentID int64, body string) error
}

// githubBackend is the real commentsBackend over per-repository clients from
// a githubreconciler.ClientCache.
type githubBackend struct {
	clients *githubreconciler.ClientCache
}

var _ commentsBackend = (*githubBackend)(nil)

func (b *githubBackend) listComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
	gh, err := b.clients.Get(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("getting client for %s/%s: %w", owner, repo, err)
	}
	opts := &github.IssueListCommentsOptions{
		Sort:        github.Ptr("created"),
		Direction:   github.Ptr("desc"),
		ListOptions: github.ListOptions{PerPage: 100},
	}
	all, truncated, err := collectNewestFirst(func(page int) ([]*github.IssueComment, int, error) {
		opts.Page = page
		comments, resp, err := gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, 0, err
		}
		return comments, resp.NextPage, nil
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		clog.WarnContextf(ctx, "comment scan for %s/%s#%d stopped after %d newest comments; older history was not scanned", owner, repo, number, len(all))
	}
	return all, nil
}

// collectNewestFirst pages through fetch — which must yield comments in
// newest-first order and the next page number (0 when exhausted) — up to
// maxCommentPages, and returns the accumulated comments in ASCENDING creation
// order, the order every Store scan relies on (pendingIn selects the newest
// marker by iterating from the tail). truncated reports that the page bound
// was hit with pages remaining.
func collectNewestFirst(fetch func(page int) ([]*github.IssueComment, int, error)) (comments []*github.IssueComment, truncated bool, err error) {
	page := 0
	for range maxCommentPages {
		batch, next, err := fetch(page)
		if err != nil {
			return nil, false, err
		}
		comments = append(comments, batch...)
		if next == 0 {
			slices.Reverse(comments)
			return comments, false, nil
		}
		page = next
	}
	slices.Reverse(comments)
	return comments, true, nil
}

func (b *githubBackend) createComment(ctx context.Context, owner, repo string, number int, body string) error {
	gh, err := b.clients.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("getting client for %s/%s: %w", owner, repo, err)
	}
	_, _, err = gh.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{Body: github.Ptr(body)})
	return err
}

func (b *githubBackend) editComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	gh, err := b.clients.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("getting client for %s/%s: %w", owner, repo, err)
	}
	_, _, err = gh.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{Body: github.Ptr(body)})
	return err
}
