/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
)

// Linear workflow state types. See
// https://developers.linear.app/docs/graphql/working-with-the-graphql-api/workflow-states
//
// IMPORTANT: Linear uses American spelling — "canceled" (one L), NOT
// "cancelled" (two L's). Both the "Cancelled" and "Duplicate" UI states
// map to type=canceled. The "Done" UI state maps to type=completed.
const (
	stateTypeCompleted = "completed"
	stateTypeCanceled  = "canceled"
)

// reconcileIssue resolves the repo target from the Linear issue, then runs the
// agent to create/update a GitHub PR. The state machine mirrors
// githubreconciler/metareconciler/issue.go.
func (r *Reconciler[Req, Resp, CB, T, PT]) reconcileIssue(ctx context.Context, issue *linearreconciler.Issue) error {
	ctx = clog.WithValues(ctx, "identifier", issue.Identifier, "title", issue.Title)

	// Check label gate.
	if r.requiredLabel != "" && !issue.HasLabel(r.requiredLabel) {
		clog.InfoContext(ctx, "Issue missing required label, skipping", "required_label", r.requiredLabel)
		return nil
	}

	if issue.State.Type == stateTypeCompleted || issue.State.Type == stateTypeCanceled {
		clog.InfoContext(ctx, "Issue is closed, skipping")
		return nil
	}

	// Terminal-state gate: if the bot already transitioned this issue to
	// StatusFailed + FailureModeNoDiff, skip reconcile entirely. The agent
	// has already decided no work is needed; running it again produces the
	// same no-op, which would post a duplicate Linear comment AND burn
	// agent inference. Operators who want to re-trigger should clear the
	// state attachment manually (or post a new task — a future change
	// could gate on description-hash deltas to allow auto-reset on edit).
	gateMgr := r.NewStateManager(issue)
	if existing, loaded, err := gateMgr.Load(ctx); err != nil {
		// Don't fail-closed on transient Load errors — fall through and
		// let the rest of reconcile run, where state mutations have their
		// own retry behaviour.
		clog.WarnContext(ctx, "Skipping no_diff terminal-state gate: load failed", "error", err)
	} else if loaded && existing.GetStatus() == StatusFailed && existing.GetFailureMode() == FailureModeNoDiff {
		clog.InfoContext(ctx, "Skipping reconcile: previously transitioned to no_diff (terminal). Clear state to retry.")
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
	var (
		usePRBranch bool
		// trigger classifies why the agent is being invoked, recorded on the
		// History entry produced by the StatusActive transition below.
		trigger string
	)
	switch {
	case changeSession.ShouldSkip():
		clog.InfoContext(ctx, "PR should be skipped, not updating")
		return nil

	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")
		trigger = TriggerMergeConflict

	case state.HitMaxCommits():
		clog.InfoContext(ctx, "PR hit turn limit")
		// ApplyTurnLimit is idempotent: re-running it on workqueue retry
		// short-circuits when the turn-limit label is already present
		// (changemanager/session.go:208-211). It also returns the PR URL,
		// which we set on state alongside StatusFailed so the failed state
		// is always self-contained — no reliance on a prior StatusActive
		// Save having populated PRURL.
		prURL, err := changeSession.ApplyTurnLimit(ctx)
		if err != nil {
			return err
		}
		mgr := r.NewStateManager(issue)
		s, _, err := mgr.Load(ctx)
		if err != nil {
			return fmt.Errorf("load state: %w", err)
		}
		s.SetStatus(StatusFailed)
		s.SetFailureMode(FailureModeMaxTurns)
		if s.GetPRURL() == "" {
			s.SetPRURL(prURL)
		}
		ctx = WithActor(ctx, r.identity)
		ctx = WithTrigger(ctx, TriggerMaxTurns)
		_, err = mgr.Save(ctx, s)
		return err

	case state.HasFindings():
		clog.InfoContext(ctx, "PR has CI findings, iterating", "findings", len(changeSession.Findings()))
		usePRBranch = true
		trigger = TriggerCIFailureIteration

	case state.HasPendingChecks():
		clog.InfoContext(ctx, "PR has pending checks, skipping", "pending_checks", changeSession.PendingChecks())
		return nil

	case state.HasNoConflicts():
		// Don't return early -- fall through to Upsert which checks if the
		// description hash changed. If unchanged, Upsert is a no-op. If the
		// issue description was edited, the agent re-runs in iteration mode.
		clog.InfoContext(ctx, "PR is green, checking for description changes")
		usePRBranch = true
		trigger = TriggerDescriptionEditIteration

	case !state.HasPR():
		clog.InfoContext(ctx, "No existing PR, creating from scratch")
		trigger = TriggerInitialRun

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

	// agentRan tracks whether the inner callback (and therefore the agent)
	// actually executed. Upsert is a no-op when nothing changed (e.g. the
	// description hash is unchanged on the HasNoConflicts path); in that
	// case we want to avoid stamping the History entry with a trigger that
	// implies an iteration that never happened.
	var agentRan bool

	// agentNoDiffNote captures the agent's intended commit message when
	// the agent runs but produces no diff. Threaded out of the inner
	// closure for the Linear bot comment so the human triaging the issue
	// sees the agent's rationale rather than a bare failure pill.
	var agentNoDiffNote string

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
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			cbs, err := r.buildCallbacks(ctx, changeSession, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			// Inject Linear-issue-id and trigger into context so bot-side
			// agent decorators (telemetry capture etc.) can correlate log
			// events without piggy-backing on the agent's request struct.
			agentCtx := WithLinearIssueID(ctx, issue.ID)
			agentCtx = WithTrigger(agentCtx, trigger)

			agentRan = true
			result, err := r.agent.Execute(agentCtx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}

			// No-diff detection: if the agent left the worktree clean, the
			// underlying worktree.Commit would return git.ErrEmptyCommit
			// and the workqueue would retry forever. Surfacing as
			// changemanager.ErrNoChanges lets the outer caller transition
			// State to StatusFailed + FailureModeNoDiff (terminal) instead.
			// The agent's intended commit message is captured in the
			// closure for the caller to record on the State.History entry.
			status, err := wt.Status()
			if err != nil {
				return "", fmt.Errorf("get worktree status: %w", err)
			}
			if status.IsClean() {
				agentNoDiffNote = result.GetCommitMessage()
				return "", changemanager.ErrNoChanges
			}
			return result.GetCommitMessage(), nil
		})
	})
	if err != nil {
		if errors.Is(err, changemanager.ErrNoChanges) {
			// "Agent produced no diff" means different things depending
			// on whether a PR already exists:
			//
			//   - Initial run (usePRBranch=false, no PR yet): agent
			//     reviewed and decided this issue needs no code changes.
			//     This is genuinely terminal — transition to
			//     StatusFailed + FailureModeNoDiff so downstream
			//     consumers see the failure mode and the workqueue
			//     stops retrying.
			//
			//   - Iteration on an existing PR (usePRBranch=true): agent
			//     re-ran (CI failure, description edit, etc.) and
			//     produced no further changes. The PR is still valid;
			//     we must NOT mark the issue failed and must NOT post a
			//     misleading "no changes needed" comment that would
			//     contradict the existing PR. Just log and return nil
			//     so the workqueue stops retrying this reconcile.
			if usePRBranch {
				clog.InfoContext(ctx, "Agent produced no diff on iteration; leaving existing PR unchanged")
				return nil
			}
			clog.InfoContext(ctx, "Agent produced no diff on initial run, transitioning to no_diff failure mode")
			return r.transitionToNoDiff(ctx, issue, agentNoDiffNote)
		}
		return fmt.Errorf("upsert PR: %w", err)
	}

	// If Upsert was a no-op (description hash unchanged on the
	// HasNoConflicts branch, etc.), the agent never ran — clear the trigger
	// so any History entry that fires (e.g. from a PRURL backfill) doesn't
	// claim an iteration that didn't happen.
	if !agentRan {
		trigger = ""
	}

	// StateManager.Save's (changed bool) gate is the gate for the bot
	// comment too: same status + same PRURL = no-op (no save, no comment,
	// no feedback loop on every reconcile). A fresh or recreated PR
	// triggers both. Save BEFORE posting the comment: if Save failed and
	// we'd already commented, the next reconcile would see no saved state
	// and comment again.
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.SetStatus(StatusActive)
	s.SetPRURL(prURL) // overwrite — fresh reconcile reflects the current PR
	ctx = WithActor(ctx, r.identity)
	ctx = WithTrigger(ctx, trigger)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if changed {
		body := fmt.Sprintf("PR created/updated: %s", prURL)
		if err := mgr.UpsertBotComment(ctx, body); err != nil {
			// State is saved; missing the comment is annoying but not
			// catastrophic. Best-effort.
			clog.WarnContext(ctx, "Failed to upsert bot comment", "error", err)
		}
	}

	clog.InfoContext(ctx, "PR created/updated", "pr_url", prURL)
	return nil
}

// transitionToNoDiff records a terminal StatusFailed + FailureModeNoDiff
// transition for an issue whose agent run produced no changes, and posts
// a Linear comment with the agent's rationale so the human triaging the
// issue knows what happened. Called from reconcileIssue when Upsert
// returns changemanager.ErrNoChanges.
//
// Returns nil so the workqueue does not retry — the agent has already
// decided the issue needs no work, and the same call would reproduce
// the same no-op. The TriggerDescriptionEditIteration path re-enters
// reconcile naturally when the human edits the issue description with
// new context.
func (r *Reconciler[Req, Resp, CB, T, PT]) transitionToNoDiff(ctx context.Context, issue *linearreconciler.Issue, note string) error {
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.SetStatus(StatusFailed)
	s.SetFailureMode(FailureModeNoDiff)
	ctx = WithActor(ctx, r.identity)
	ctx = WithTrigger(ctx, TriggerNoDiff)

	// Order matters: UpsertBotComment must run BEFORE Save. The framework's
	// linearreconciler.StateManager only persists the comment-tracking
	// commentID alongside the state attachment on the next Save call (see
	// linearreconciler/state.go). Saving first would persist an empty
	// commentID, then subsequent reconciles that re-enter this path would
	// post a fresh comment instead of updating the existing one. The
	// terminal-state gate at the top of reconcileIssue already prevents
	// most re-entry, but the order swap here is defence-in-depth.
	body := "I reviewed this issue but determined no code changes are needed."
	if note != "" {
		body = "I reviewed this issue but determined no code changes are needed:\n\n> " + note
	}
	if err := mgr.UpsertBotComment(ctx, body); err != nil {
		// Comment failure is logged but not fatal — the state transition
		// is the more important record. A failed comment also leaves
		// commentID empty in memory, which is fine; the state still
		// records the failure mode for downstream consumers.
		clog.WarnContext(ctx, "Failed to upsert no-diff bot comment", "error", err)
	}

	if _, err := mgr.Save(ctx, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
