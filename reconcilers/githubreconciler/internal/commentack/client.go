/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commentack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
)

// API reads a pull request's comments and adds 👀 reactions. Client is the
// GitHub implementation; tests use a fake.
type API interface {
	// List returns the conversation comments and the inline review comments of
	// a pull request, and the date of its head commit. The date is the zero
	// time when the pull request has no commit. When it has a commit whose
	// date cannot be read, List returns no comments, so none can get a 👀.
	List(ctx context.Context, owner, repo string, number int) ([]Comment, time.Time, error)
	// AddEyes puts a 👀 on the comment with the given GraphQL node ID. Adding
	// one that is already there is not an error.
	AddEyes(ctx context.Context, subjectID string) error
}

// Client is the GitHub GraphQL implementation of API. It holds no state of its
// own, so it is safe for concurrent use.
type Client struct {
	gql *graphqlclient.GraphQLClient
}

var _ API = (*Client)(nil)

// NewClient returns a Client that makes its calls with gh.
func NewClient(gh *github.Client) *Client {
	return &Client{gql: graphqlclient.NewGraphQLClient(gh)}
}

// gqlComment selects the fields both kinds of comment share. Conversation
// comments and inline review comments have the same field names for these.
type gqlComment struct {
	Id                string
	CreatedAt         githubv4.DateTime
	IsMinimized       bool
	AuthorAssociation string
	Body              string
	Author            struct {
		Typename string `graphql:"__typename"`
	}
	ReactionGroups []struct {
		Content  string
		Reactors struct {
			Nodes []struct {
				Typename string `graphql:"__typename"`
			}
		} `graphql:"reactors(first: 10)"`
	}
}

// List implements API.
func (c *Client) List(ctx context.Context, owner, repo string, number int) ([]Comment, time.Time, error) {
	if number < 0 || number > math.MaxInt32 {
		return nil, time.Time{}, fmt.Errorf("pull request number %d outside int32", number)
	}
	// Only the newest ten comments of each thread are read. Each comment brings
	// its own list of reactors, so reading deeper costs about a hundred
	// rate-limit points per call, against about twelve for this shape.
	var q struct {
		Repository struct {
			PullRequest *struct {
				Comments struct {
					Nodes []gqlComment
				} `graphql:"comments(last: 100)"`
				ReviewThreads struct {
					Nodes []struct {
						IsResolved bool
						Comments   struct {
							Nodes []gqlComment
						} `graphql:"comments(last: 10)"`
					}
				} `graphql:"reviewThreads(first: 100)"`
				// The date is read as a string so that an empty or broken one can be
				// told apart from a failed query.
				Commits struct {
					Nodes []struct {
						Commit struct {
							CommittedDate string
						}
					}
				} `graphql:"commits(last: 1)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	if err := c.gql.Query(ctx, "CommentAckList", &q, map[string]any{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(number),
	}); err != nil {
		return nil, time.Time{}, fmt.Errorf("querying pull request comments: %w", err)
	}
	pr := q.Repository.PullRequest
	if pr == nil {
		return nil, time.Time{}, nil
	}
	var headCommitted time.Time
	if len(pr.Commits.Nodes) > 0 {
		// The zero time means no cutoff, so a date that cannot be read must not
		// fall back to it: every comment in the window would get a 👀.
		committed, err := time.Parse(time.RFC3339, pr.Commits.Nodes[0].Commit.CommittedDate)
		if err != nil {
			clog.WarnContextf(ctx, "Skipping comment acks: cannot read the head commit date of %s/%s#%d: %v", owner, repo, number, err)
			return nil, time.Time{}, nil
		}
		headCommitted = committed
	}
	nodes := pr.Comments.Nodes
	comments := make([]Comment, 0, len(nodes))
	for _, n := range nodes {
		comments = append(comments, toComment(n))
	}
	// The bot only acts on open threads, so a comment in a resolved one gets no 👀.
	for _, thread := range pr.ReviewThreads.Nodes {
		if thread.IsResolved {
			continue
		}
		for _, n := range thread.Comments.Nodes {
			c := toComment(n)
			c.Inline = true
			comments = append(comments, c)
		}
	}
	return comments, headCommitted, nil
}

// toComment converts a GraphQL node into a Comment. A deleted account comes back
// with an empty type, which stays empty in AuthorType.
func toComment(n gqlComment) Comment {
	c := Comment{
		NodeID:            n.Id,
		AuthorType:        n.Author.Typename,
		AuthorAssociation: n.AuthorAssociation,
		Body:              n.Body,
		CreatedAt:         n.CreatedAt.Time,
		IsMinimized:       n.IsMinimized,
	}
	for _, g := range n.ReactionGroups {
		if g.Content != string(githubv4.ReactionContentEyes) {
			continue
		}
		for _, r := range g.Reactors.Nodes {
			if r.Typename == botType {
				c.EyesFromBot = true
			}
		}
	}
	return c
}

// AddEyes implements API.
func (c *Client) AddEyes(ctx context.Context, subjectID string) error {
	var m struct {
		AddReaction struct {
			ClientMutationId string
		} `graphql:"addReaction(input: $input)"`
	}
	return c.gql.Mutate(ctx, "CommentAckAddReaction", &m, githubv4.AddReactionInput{
		SubjectID: githubv4.ID(subjectID),
		Content:   githubv4.ReactionContentEyes,
	}, nil)
}

// Ack puts a 👀 on each comment of the pull request that Pending selects. Only
// comments left within the window get one, and conversation comments only when
// they were left after the head commit, the same cutoff the bot uses to pick
// conversation comments to act on. Comments in resolved review threads get
// none. Ack sets o.Since
// to the head commit date List returns, replacing any Since the caller set. A
// failure on one comment does not stop the rest; the failures are returned
// together. The caller treats the error as advisory: a missed 👀 is tried again
// on the next reconcile.
func Ack(ctx context.Context, api API, owner, repo string, number int, o Options) error {
	comments, headCommitted, err := api.List(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("listing comments: %w", err)
	}
	o.Since = headCommitted
	var errs []error
	for _, id := range Pending(comments, o) {
		if err := api.AddEyes(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("reacting to %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
