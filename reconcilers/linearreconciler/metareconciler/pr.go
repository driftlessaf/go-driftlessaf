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
)

// HandlePREvent processes a GitHub PR URL by extracting the Linear issue ID
// from the PR body and re-queuing it. This enables the CI feedback loop:
// PR CI fails → PR event → extract Linear issue ID → re-queue → iterate.
//
// The re-queue key is the Linear issue UUID (not the human identifier like
// ENG-123) because the linear-events trampoline keys workqueue items by UUID
// (see linear-metareconciler module: extension_key = "issueid"). Using the
// UUID keeps the two paths consistent so the workqueue can dedupe.
func (r *Reconciler[Req, Resp, CB]) HandlePREvent(ctx context.Context, prURL string) (*workqueue.ProcessResponse, error) {
	ctx = clog.WithValues(ctx, "pr_url", prURL)

	res, err := githubreconciler.ParseURL(prURL)
	if err != nil {
		return nil, workqueue.NonRetriableError(
			fmt.Errorf("parsing PR URL: %w", err),
			"invalid GitHub PR URL",
		)
	}
	if res.Type != githubreconciler.ResourceTypePullRequest {
		clog.InfoContext(ctx, "Not a pull request URL, skipping")
		return &workqueue.ProcessResponse{}, nil
	}

	gh, err := r.githubClients.Get(ctx, res.Owner, res.Repo)
	if err != nil {
		return nil, fmt.Errorf("get GitHub client: %w", err)
	}

	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return nil, fmt.Errorf("fetch PR: %w", err)
	}

	// Most PRs in the repo aren't ours; Extract returns an error whenever the
	// PRData marker isn't present. Log at Info with the underlying error so
	// genuine schema-drift cases are still investigable, but don't escalate.
	data, err := r.changeManager.Extract(pr.GetBody())
	if err != nil {
		clog.InfoContext(ctx, "No PRData marker in PR body, skipping", "error", err)
		return &workqueue.ProcessResponse{}, nil
	}
	if data.LinearIssueID == "" {
		// The marker WAS present and parsed cleanly, but the embedded data
		// has no LinearIssueID. That's a real schema bug worth surfacing.
		clog.WarnContext(ctx, "PRData marker present but LinearIssueID is empty, skipping")
		return &workqueue.ProcessResponse{}, nil
	}

	clog.InfoContext(ctx, "Re-queuing Linear issue from PR event", "linear_issue_id", data.LinearIssueID)
	return &workqueue.ProcessResponse{
		QueueKeys: []*workqueue.QueueKeyRequest{{Key: data.LinearIssueID}},
	}, nil
}
