/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// reconcileIssue processes an issue URL and runs the agent to create/update a PR.
func (r *Reconciler[Req, Resp, CB]) reconcileIssue(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	log := clog.FromContext(ctx)

	// Fetch the issue
	issue, _, err := gh.Issues.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}

	// Create a change session for the PR (needed for skip label check and PR cleanup)
	changeSession, err := r.changeManager.NewSession(ctx, gh, res)
	switch {
	case err != nil:
		return fmt.Errorf("create change session: %w", err)

	case changeSession.HasSkipLabel():
		log.Info("PR has skip label, not updating")
		return nil

	case r.requiredLabel != "" && !hasLabel(issue, r.requiredLabel):
		log.With("required_label", r.requiredLabel).Info("Issue missing required label, closing any outstanding PRs")
		return changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue no longer has the required label.")

	case issue.GetState() == "closed":
		log.Info("Issue is closed, closing any outstanding PRs")
		return changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue was closed.")

	case changeSession.HasFindings():
		log.With("findings", len(changeSession.Findings())).Info("PR has CI failures, iterating to address findings")

	case changeSession.HasPendingChecks():
		log.With("pending_checks", changeSession.PendingChecks()).Info("PR has pending checks, skipping")
		return nil
	}

	// Create/update the PR with the changes
	prURL, err := changeSession.Upsert(ctx, &PRData{
		Identity:      r.identity,
		IssueURL:      issue.GetHTMLURL(),
		IssueNumber:   issue.GetNumber(),
		IssueBodyHash: sha256.Sum256([]byte(issue.GetBody())),
	}, false, r.prLabels, func(ctx context.Context, branchName string) error {
		cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
		if err != nil {
			return fmt.Errorf("get clone manager: %w", err)
		}

		// If we have findings (CI failures), check out the PR branch to iterate.
		// Otherwise, start fresh from the main branch.
		var lease *clonemanager.Lease
		if changeSession.HasFindings() {
			log.With("branch", branchName).Info("Acquiring clone lease for pull request")
			lease, err = cloneMgr.LeaseRef(ctx, res, branchName)
		} else {
			log.Info("Acquiring clone lease for fresh run")
			lease, err = cloneMgr.Lease(ctx, res)
		}
		if err != nil {
			return fmt.Errorf("acquire lease: %w", err)
		}
		defer func() {
			if err := lease.Return(ctx); err != nil {
				log.With("error", err).Warn("Failed to return lease")
			}
		}()

		// Run the agent and push changes
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			request := r.buildRequest(issue, changeSession)
			callbacks := r.buildCallbacks(wt, changeSession)

			result, err := r.agent.Execute(ctx, request, callbacks)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			return result.GetCommitMessage(), nil
		})
	})
	if err != nil {
		return fmt.Errorf("upsert PR: %w", err)
	}

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}

// hasLabel checks if an issue has a specific label.
func hasLabel(issue *github.Issue, label string) bool {
	for _, l := range issue.Labels {
		if l.GetName() == label {
			return true
		}
	}
	return false
}
