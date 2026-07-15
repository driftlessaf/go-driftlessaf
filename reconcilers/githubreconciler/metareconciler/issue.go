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

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/statemachine"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
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
		if changeSession.HasSkipLabel() {
			clog.InfoContext(ctx, "PR has skip label, not updating to preserve manual changes", "pr", changeSession.PRNumber())
		} else {
			clog.InfoContext(ctx, "PR is assigned to humans, not updating to avoid stomping their work", "pr", changeSession.PRNumber(), "assignees", changeSession.Assignees())
		}
		return nil

	case changeSession.IssueHasSkipLabel(issue):
		clog.InfoContext(ctx, "Issue has skip label, leaving it and any PR alone")
		return nil

	case r.requiredLabel != "" && !hasLabel(issue, r.requiredLabel):
		clog.InfoContext(ctx, "Issue missing required label, closing any outstanding PRs", "required_label", r.requiredLabel)
		r.giveUp.Clear(ctx, changeSession)
		prURL := changeSession.PRURL()
		if err := changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue no longer has the required label."); err != nil {
			return err
		}
		// Only a PR actually closed just now is a transition; a labelless
		// issue with no PR re-reconciling is not.
		if state.HasPR() {
			r.emitTransition(ctx, issue, prURL,
				statemachine.StatusFailed, statemachine.FailureModePRClosed, TriggerRequiredLabelRemoved)
		}
		return nil

	case issue.GetState() == "closed":
		clog.InfoContext(ctx, "Issue is closed, closing any outstanding PRs")
		r.giveUp.Clear(ctx, changeSession)
		prURL := changeSession.PRURL()
		if err := changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue was closed."); err != nil {
			return err
		}
		// With no persisted prior state this re-emits if a closed issue
		// reconciles again (rare: closed issues stop generating events);
		// latest-transition consumers are unaffected since the terminal
		// status is unchanged.
		to, mode := closedIssueTransition(state.HasPR(), issue.GetStateReason())
		if !state.HasPR() {
			prURL = ""
		}
		r.emitTransition(ctx, issue, prURL, to, mode, TriggerIssueClosed)
		return nil

	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		clog.InfoContext(ctx, "PR hit turn limit")
		// The label edge is the transition: ApplyTurnLimit is idempotent, so
		// only the reconcile that newly applies the label emits.
		newlyLimited := !changeSession.HasTurnLimitLabel()
		prURL, err := changeSession.ApplyTurnLimit(ctx)
		if err == nil && newlyLimited {
			r.emitTransition(ctx, issue, prURL,
				statemachine.StatusFailed, statemachine.FailureModeMaxTurns, statemachine.TriggerMaxTurns)
		}
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
		newlyReady := !changeSession.HasReadyForReviewLabel()
		prURL, err := changeSession.ApplyReadyForReview(ctx)
		if err != nil {
			return fmt.Errorf("apply ready-for-review: %w", err)
		}
		if newlyReady {
			// Still active — the PR now waits on humans, and time since this
			// transition measures how long it has waited.
			r.emitTransition(ctx, issue, prURL,
				statemachine.StatusActive, "", TriggerReadyForReview)
		}
		// The PR is green: drop any give-up comment from a prior iteration, since
		// the PR recovered without the agent needing to push a fix. Clear is a
		// no-op when no comment exists, so this is safe to run on every green pass.
		r.giveUp.Clear(ctx, changeSession)
		return nil

	case !state.HasPR():
		log.Info("No existing PR, creating from scratch")

	default:
		log.With("state", state).Warn("Unexpected state combination")
	}

	// Announce work has started on the issue before the first PR appears. Only
	// when no PR exists yet, so new commits on an open PR never retrigger it; the
	// marker dedup keeps it to one comment across repeated no-PR reconciles (a
	// first attempt may end in ErrNoChanges or a transient agent error).
	if !state.HasPR() {
		r.startComment.surface(ctx, changeSession)
	}

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, issue, changeSession)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// Captured from the agent execution inside the Upsert closure. Used after
	// Upsert to derive PR labels (WithPRLabelsFromResult) and to surface a
	// give-up comment (WithGiveUpComment). agentRan gates both uses: Upsert skips
	// the closure when the PR is already up to date, leaving result as the zero
	// value, so neither must see it then.
	var result Resp
	var agentRan bool

	// Carry the issue's labels onto the PR when configured, so labels added to
	// the issue (e.g. to enable optional review) propagate situationally rather
	// than always. Merged with the fixed prLabels; deduped so a label present in
	// both is stamped once.
	prLabels := r.prLabelsForIssue(issue)

	// Create/update the PR with the changes. prData is passed by pointer and
	// the body template renders only after the closure below runs, so fields
	// set post-execution (ReasoningSummary) are visible to the template.
	prData := &PRData[Req]{
		Identity:      r.identity,
		IssueURL:      issue.GetHTMLURL(),
		IssueNumber:   issue.GetNumber(),
		IssueBodyHash: sha256.Sum256([]byte(issue.GetBody())),
		Request:       request,
	}
	prURL, err := changeSession.Upsert(ctx, prData, false, prLabels, func(ctx context.Context, branchName string) error {
		// Tee the agent's completed trace so the PR body template can render
		// a rationale summary via {{.ReasoningSummary}} (see
		// ReasoningSummarySnippet): per-action tool-call reasoning when
		// present, falling back to extended-thinking blocks. No-op when the
		// run produced neither.
		ctx, captured := agenttrace.CaptureTrace[Resp](ctx)
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
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			cbs, err := r.buildCallbacks(ctx, changeSession, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			result, err = r.agent.Execute(ctx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			agentRan = true
			prData.ReasoningSummary = agenttrace.SummarizeTraceReasoning(captured(), reasoningSummaryMaxChars)

			// Check if the agent left the worktree clean (no file changes).
			// Return ErrNoChanges so Upsert can propagate it to the caller.
			status, err := wt.Status()
			if err != nil {
				return "", fmt.Errorf("get worktree status: %w", err)
			}
			if status.IsClean() {
				return "", changemanager.ErrNoChanges
			}

			return result.GetCommitMessage(), nil
		})
	})
	if err != nil {
		if errors.Is(err, changemanager.ErrNoChanges) {
			log.Info("No changes after agent execution, nothing to commit")
			if agentRan {
				// SurfaceResult applies the give-up label when the result
				// carries an explanation; the session's label cache reflects
				// that immediately, so the before/after pair is the "newly
				// gave up" edge to emit on.
				//
				// This edge only fires for bots that wire WithGiveUpComment
				// (today: linear-materializer). Without it SurfaceResult
				// no-ops, the label never appears, and a no-diff run emits
				// nothing — on every retry, not just the first.
				gaveUpBefore := changeSession.HasGaveUpLabel()
				r.giveUp.SurfaceResult(ctx, changeSession, result)
				if !gaveUpBefore && changeSession.HasGaveUpLabel() {
					r.emitTransition(ctx, issue, changeSession.PRURL(),
						statemachine.StatusFailed, statemachine.FailureModeNoDiff, statemachine.TriggerNoDiff)
				}
			}
			return nil
		}
		return fmt.Errorf("upsert PR: %w", err)
	}

	// The agent pushed a fix: clear any stale give-up comment from a prior
	// iteration where it had nothing to do.
	r.giveUp.Clear(ctx, changeSession)

	// Every push is a genuine transition (back) to active: the initial run
	// that created the PR, a findings iteration, or a post-conflict
	// regeneration. Upsert skips the agent closure when the PR is already up
	// to date, so agentRan gates re-observations out. state still reflects
	// the pre-Upsert session, so HasPR distinguishes the initial run.
	//
	// An empty prURL with a nil error is the closeOnEmptyDiff path: the agent
	// pushed commits that net to zero against base, so Upsert closed the PR
	// (or never opened one). Emitting active there would pin the issue
	// "active" on the overlay with no PR that could ever move it again —
	// every genuine create/update path returns a non-empty URL.
	if agentRan && prURL != "" {
		r.emitTransition(ctx, issue, prURL,
			statemachine.StatusActive, "", upsertTrigger(!state.HasPR(), usePRBranch, state.NeedsRebase()))
	}

	// Assign the PR to the issue creator so they can easily find it.
	if creator != "" {
		if err := changeSession.AddAssignees(ctx, []string{creator}); err != nil {
			log.With("error", err).Warn("Failed to assign PR to issue creator")
		}
	}

	// Stamp result-derived labels on the PR (opt-in via WithPRLabelsFromResult).
	// Skipped when the agent did not run (PR already up to date): result is the
	// zero value then, so deriving labels from it would be meaningless.
	if r.prLabelsFromResult != nil && agentRan {
		if err := changeSession.AddLabels(ctx, r.prLabelsFromResult(result)); err != nil {
			log.With("error", err).Warn("Failed to add result-derived labels to PR")
		}
	}

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}

// prLabelsForIssue returns the labels to stamp on the PR for this issue: the
// fixed prLabels, plus every label on the issue when copyIssueLabels is set.
// Duplicates are collapsed so a label present in both is returned once. When
// copyIssueLabels is off it returns the fixed prLabels unchanged.
func (r *Reconciler[Req, Resp, CB]) prLabelsForIssue(issue *github.Issue) []string {
	if !r.copyIssueLabels {
		return r.prLabels
	}
	seen := make(map[string]struct{}, len(r.prLabels)+len(issue.Labels))
	labels := make([]string, 0, len(r.prLabels)+len(issue.Labels))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		labels = append(labels, name)
	}
	for _, l := range r.prLabels {
		add(l)
	}
	for _, l := range issue.Labels {
		add(l.GetName())
	}
	return labels
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
