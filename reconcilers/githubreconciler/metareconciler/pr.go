/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v75/github"
	"github.com/shurcooL/githubv4"
)

// reconcilePullRequest handles PR events by finding linked issues with the required
// label and queueing them for re-processing.
func (r *Reconciler[Req, Resp, CB]) reconcilePullRequest(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	log := clog.FromContext(ctx)

	// If no required label is configured, there's nothing to do for PRs
	if r.requiredLabel == "" {
		log.Debug("No required label configured, skipping PR")
		return nil
	}

	// Find linked issues with the required label
	issueURLs, err := findLinkedIssuesWithLabel(ctx, gh, res.Owner, res.Repo, res.Number, r.requiredLabel)
	if err != nil {
		return fmt.Errorf("find linked issues: %w", err)
	}

	if len(issueURLs) == 0 {
		log.Debug("No linked issues with required label found")
		return nil
	}

	log.With("linked_issues", len(issueURLs)).Info("Queueing linked issues for processing")

	// Queue all linked issues for processing
	keys := make([]workqueue.QueueKey, 0, len(issueURLs))
	for _, url := range issueURLs {
		keys = append(keys, workqueue.QueueKey{Key: url})
	}
	return workqueue.QueueKeys(keys...)
}

// findLinkedIssuesWithLabel queries for issues linked to a PR via "closes" keywords
// and returns URLs for issues that have the specified label.
func findLinkedIssuesWithLabel(ctx context.Context, gh *github.Client, owner, repo string, prNumber int, label string) ([]string, error) {
	gqlClient := githubv4.NewClient(gh.Client())

	var query struct {
		Repository struct {
			PullRequest struct {
				ClosingIssuesReferences struct {
					Nodes []struct {
						URL    string
						Labels struct {
							Nodes []struct {
								Name string
							}
						} `graphql:"labels(first: 20)"`
					}
				} `graphql:"closingIssuesReferences(first: 10)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	variables := map[string]any{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(prNumber),
	}

	if err := gqlClient.Query(ctx, &query, variables); err != nil {
		return nil, fmt.Errorf("graphql query: %w", err)
	}

	// Filter for issues with the required label
	var urls []string
	for _, issue := range query.Repository.PullRequest.ClosingIssuesReferences.Nodes {
		for _, l := range issue.Labels.Nodes {
			if l.Name == label {
				urls = append(urls, issue.URL)
				break
			}
		}
	}

	return urls, nil
}
