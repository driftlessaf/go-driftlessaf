/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"github.com/shurcooL/githubv4"
)

type commitBudgetConnection struct {
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
	Nodes []struct {
		Commit struct {
			Parents struct {
				TotalCount int
			} `graphql:"parents(first: 2)"`
		}
	}
}

func countNonMergeCommits(ctx context.Context, client *graphqlclient.GraphQLClient, owner, repo string, prNumber int) (int, error) {
	var query struct {
		Repository struct {
			PullRequest struct {
				Commits commitBudgetConnection `graphql:"commits(first: 100)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	variables := map[string]any{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(prNumber),
	}
	if err := client.Query(ctx, "CountNonMergeCommits", &query, variables); err != nil {
		return 0, fmt.Errorf("listing pull request commits: %w", err)
	}

	count := 0
	commits := query.Repository.PullRequest.Commits
	for {
		for _, node := range commits.Nodes {
			if node.Commit.Parents.TotalCount < 2 {
				count++
			}
		}
		if !commits.PageInfo.HasNextPage {
			return count, nil
		}

		var page struct {
			Repository struct {
				PullRequest struct {
					Commits commitBudgetConnection `graphql:"commits(first: 100, after: $cursor)"`
				} `graphql:"pullRequest(number: $number)"`
			} `graphql:"repository(owner: $owner, name: $repo)"`
		}
		variables["cursor"] = githubv4.String(commits.PageInfo.EndCursor)
		if err := client.Query(ctx, "CountNonMergeCommitsPage", &page, variables); err != nil {
			return 0, fmt.Errorf("listing pull request commits: %w", err)
		}
		commits = page.Repository.PullRequest.Commits
	}
}
