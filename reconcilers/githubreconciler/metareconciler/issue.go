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
	"github.com/google/go-github/v84/github"
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
	if err != nil {
		return fmt.Errorf("create change session: %w", err)
	}

	// The issue creator is a bot-managed assignee: assigning the PR to them
	// should not cause ShouldSkip to return true.
	creator := issue.GetUser().GetLogin()

	state := changeSession.State()
	var usePRBranch bool
	switch {
	case changeSession.ShouldSkip(creator):
		log.Info("PR should be skipped, not updating")
		return nil

	case r.requiredLabel != "" && !hasLabel(issue, r.requiredLabel):
		log.With("required_label", r.requiredLabel).Info("Issue missing required label, closing any outstanding PRs")
		return changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue no longer has the required label.")

	case issue.GetState() == "closed":
		log.Info("Issue is closed, closing any outstanding PRs")
		return changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue was closed.")

	case state.NeedsRebase():
		log.Info("PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		log.Info("PR hit turn limit")
		_, err := changeSession.ApplyTurnLimit(ctx)
		return err

	// Historically we delayed here (commented code below), but in high-volume
	// repositories github can take a long time to compute mergeability, so we
	// are choosing to optimistically proceed as-if there isn't a rebase needed
	// when github has not computed mergeability.
	// case state.IsUnknown():
	// 	log.Info("PR merge status unknown, requeuing to check again shortly")
	// 	return workqueue.RequeueAfter(2 * time.Minute)

	case state.HasFindings():
		log.With("findings", len(changeSession.Findings())).Info("PR has CI findings, iterating")
		usePRBranch = true

	case state.HasPendingChecks():
		log.With("pending_checks", changeSession.PendingChecks()).Info("PR has pending checks, skipping")
		return nil

	case state.HasNoConflicts():
		log.Info("PR is green, leaving it for human review")
		return nil

	case !state.HasPR():
		log.Info("No existing PR, creating from scratch")

	default:
		log.With("state", state).Warn("Unexpected state combination")
	}

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, issue, changeSession)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// Create/update the PR with the changes
	prURL, err := changeSession.Upsert(ctx, &PRData[Req]{
		Identity:      r.identity,
		IssueURL:      issue.GetHTMLURL(),
		IssueNumber:   issue.GetNumber(),
		IssueBodyHash: sha256.Sum256([]byte(issue.GetBody())),
		Request:       request,
	}, false, r.prLabels, func(ctx context.Context, branchName string) error {
		cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
		if err != nil {
			return fmt.Errorf("get clone manager: %w", err)
		}

		// Lease based on current state:
		// - CI failures on a mergeable PR: lease PR branch for iteration
		// - Otherwise (no PR, needs rebase, or fresh run): lease default branch
		var lease *clonemanager.Lease
		if usePRBranch {
			log.With("branch", branchName).Info("Acquiring clone lease for pull request branch")
			lease, err = cloneMgr.LeaseRef(ctx, res, branchName,
				clonemanager.WithCommitDepth(changeSession.CommitCount()+1))
		} else {
			log.Info("Acquiring clone lease for default branch")
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
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, _ *gogit.Worktree) (string, error) {
			cbs, err := r.buildCallbacks(ctx, changeSession, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			result, err := r.agent.Execute(ctx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			return result.GetCommitMessage(), nil
		})
	})
	if err != nil {
		return fmt.Errorf("upsert PR: %w", err)
	}

	// Assign the PR to the issue creator so they can easily find it.
	if creator != "" {
		if err := changeSession.AddAssignees(ctx, []string{creator}); err != nil {
			log.With("error", err).Warn("Failed to assign PR to issue creator")
		}
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
