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
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
)

// Linear workflow state types. See
// https://developers.linear.app/docs/graphql/working-with-the-graphql-api/workflow-states
const (
	stateTypeCompleted = "completed"
	stateTypeCancelled = "cancelled"
)

// reconcileIssue resolves the repo target from the Linear issue, then runs the
// agent to create/update a GitHub PR. The state machine mirrors
// githubreconciler/metareconciler/issue.go.
func (r *Reconciler[Req, Resp, CB]) reconcileIssue(ctx context.Context, issue *linearreconciler.Issue) error {
	ctx = clog.WithValues(ctx, "identifier", issue.Identifier, "title", issue.Title)

	// Check label gate.
	if r.requiredLabel != "" && !issue.HasLabel(r.requiredLabel) {
		clog.InfoContext(ctx, "Issue missing required label, skipping", "required_label", r.requiredLabel)
		return nil
	}

	if issue.State.Type == stateTypeCompleted || issue.State.Type == stateTypeCancelled {
		clog.InfoContext(ctx, "Issue is closed, skipping")
		return nil
	}

	// Resolve the GitHub repo target — first via upstream bot state
	// attachment, then via the optional fallback resolver if configured.
	target, err := r.resolveRepoTarget(ctx, issue)
	if err != nil {
		return fmt.Errorf("resolve repo target: %w", err)
	}

	owner, repo, err := splitOwnerRepo(target.Repo)
	if err != nil {
		return fmt.Errorf("parse repo target: %w", err)
	}

	ctx = clog.WithValues(ctx, "owner", owner, "repo", repo)

	// Construct a Resource using ResourceTypePath so that changemanager
	// derives a branch name from the Linear issue UUID (which is immutable,
	// unlike team-prefixed identifiers like ENG-123).
	res := &githubreconciler.Resource{
		Owner: owner,
		Repo:  repo,
		Type:  githubreconciler.ResourceTypePath,
		Path:  "linear-" + issue.ID,
		Ref:   "main",
	}

	// Get a GitHub client for this repo.
	gh, err := r.githubClients.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get GitHub client: %w", err)
	}

	// Create a change session which queries PR state via GraphQL.
	changeSession, err := r.changeManager.NewSession(ctx, gh, res)
	if err != nil {
		return fmt.Errorf("create change session: %w", err)
	}
	state := changeSession.State()
	var usePRBranch bool
	switch {
	case changeSession.ShouldSkip():
		clog.InfoContext(ctx, "PR should be skipped, not updating")
		return nil

	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		clog.InfoContext(ctx, "PR hit turn limit")
		_, err := changeSession.ApplyTurnLimit(ctx)
		return err

	case state.HasFindings():
		clog.InfoContext(ctx, "PR has CI findings, iterating", "findings", len(changeSession.Findings()))
		usePRBranch = true

	case state.HasPendingChecks():
		clog.InfoContext(ctx, "PR has pending checks, skipping", "pending_checks", changeSession.PendingChecks())
		return nil

	case state.HasNoConflicts():
		// Don't return early -- fall through to Upsert which checks if the
		// description hash changed. If unchanged, Upsert is a no-op. If the
		// issue description was edited, the agent re-runs in iteration mode.
		clog.InfoContext(ctx, "PR is green, checking for description changes")
		usePRBranch = true

	case !state.HasPR():
		clog.InfoContext(ctx, "No existing PR, creating from scratch")

	default:
		// Don't fall through to Upsert on an unrecognised state combination —
		// running the agent on stale context could clobber an existing PR.
		// Surface the gap loudly instead so it shows up in alerts.
		clog.ErrorContext(ctx, "Unexpected PR state combination, skipping reconciliation", "state", state)
		return nil
	}

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, issue, changeSession)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// Create/update the PR with the changes.
	prURL, err := changeSession.Upsert(ctx, &PRData[Req]{
		Identity:         r.identity,
		LinearIssueID:    issue.ID,
		LinearIdentifier: issue.Identifier,
		DescriptionHash:  sha256.Sum256([]byte(issue.Description)),
		Request:          request,
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
			clog.InfoContext(ctx, "Acquiring clone lease for pull request branch", "branch", branchName)
			lease, err = cloneMgr.LeaseRef(ctx, res, branchName,
				clonemanager.WithCommitDepth(changeSession.CommitCount()+1))
		} else {
			clog.InfoContext(ctx, "Acquiring clone lease for default branch")
			lease, err = cloneMgr.Lease(ctx, res)
		}
		if err != nil {
			return fmt.Errorf("acquire lease: %w", err)
		}
		defer func() {
			if err := lease.Return(ctx); err != nil {
				// A failed lease return leaks a clone on disk; left
				// unchecked it eventually exhausts the pod's filesystem.
				// Log at Error so the alert pipeline picks it up.
				clog.ErrorContext(ctx, "Failed to return lease (clone may leak)", "error", err)
			}
		}()

		// Run the agent and push changes.
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

	// Only update Linear status if the PR URL is new or changed.
	// Updating status on every reconciliation creates a feedback loop:
	// status update → attachment/comment event → re-reconcile → repeat.
	sm := r.linearClient.NewStateManager(issue)
	var existingState MaterializerState
	loaded, loadErr := sm.Load(ctx, &existingState)
	if loadErr != nil {
		// Treating Load failure as "no prior state" would re-post the bot
		// comment on every retry until Load eventually succeeds — exactly
		// the spam loop the surrounding logic exists to prevent. Return
		// retriable so the workqueue backs off without leaking comments.
		return fmt.Errorf("load materializer state: %w", loadErr)
	}
	if !loaded || existingState.PRURL != prURL {
		existingState.PRURL = prURL
		existingState.Status = StatusActive
		// Save BEFORE posting the comment: if Save fails and we'd already
		// commented, the next reconcile would see no saved state and
		// comment again. Returning here keeps state and comment in sync.
		if err := sm.Save(ctx, &existingState); err != nil {
			return fmt.Errorf("save materializer state: %w", err)
		}
		body := fmt.Sprintf("PR created/updated: %s", prURL)
		if err := sm.UpsertBotComment(ctx, body); err != nil {
			// State is saved; missing the comment is annoying but not
			// catastrophic. Best-effort.
			clog.WarnContext(ctx, "Failed to upsert bot comment", "error", err)
		}
	}

	clog.InfoContext(ctx, "PR created/updated", "pr_url", prURL)
	return nil
}
