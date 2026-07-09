/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
)

// reconcilePath handles path resources by running the analyzer and agent.
func (r *PRReconciler[Req, Resp, CB]) reconcilePath(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	log := clog.FromContext(ctx)

	// Create a change session for the PR
	session, err := r.changeManager.NewSession(ctx, gh, res)
	if err != nil {
		return fmt.Errorf("create change session: %w", err)
	}
	state := session.State()
	var usePRBranch bool
	switch {
	case session.ShouldSkip():
		if session.HasSkipLabel() {
			clog.InfoContext(ctx, "PR has skip label, not updating to preserve manual changes", "pr", session.PRNumber())
		} else {
			clog.InfoContext(ctx, "PR is assigned to humans, not updating to avoid stomping their work", "pr", session.PRNumber(), "assignees", session.Assignees())
		}
		return nil

	// If the PR is not mergeable, ignore everything about the existing PR
	// and start from scratch on the default branch.
	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		// Unresolved review feedback grants a fresh commit budget (a no-op
		// unless WithDynamicCommitBudget is enabled). If the limit is still
		// hit after the reset, the turn limit stands.
		if session.HasUnresolvedReviews() {
			session.ResetCommitBudget(ctx)
		}
		if session.State().HitMaxCommits() {
			clog.InfoContext(ctx, "PR hit turn limit")
			_, err := session.ApplyTurnLimit(ctx)
			return err
		}
		log.Info("PR hit turn limit but has unresolved reviews, iterating with fresh commit budget")
		usePRBranch = true

	case state.HasFindings():
		log.With("findings", len(session.Findings())).Info("PR has CI findings, iterating")
		usePRBranch = true

	case state.HasPendingChecks():
		log.With("pending_checks", session.PendingChecks()).Info("PR has pending checks, skipping")
		return nil

	// Mergeability not yet computed, with nothing else to act on (findings and
	// pending checks handled above). Only opted-in reconcilers requeue to
	// re-check rather than resetting the PR from the default branch; the case is
	// gated on the option so others fall through to the historical behavior.
	case state.IsUnknown() && r.unknownMergeabilityRequeueAfter > 0:
		log.With("after", r.unknownMergeabilityRequeueAfter).Info("PR mergeability still being computed by GitHub, requeuing")
		return workqueue.RequeueAfter(r.unknownMergeabilityRequeueAfter)

	case state.HasNoConflicts():
		log.Info("PR is green, leaving it for human review")
		if _, err := session.ApplyReadyForReview(ctx); err != nil {
			return fmt.Errorf("apply ready-for-review: %w", err)
		}
		// The PR is green: drop any give-up comment from a prior iteration, since
		// the PR recovered (e.g. a blocking dependency landed elsewhere) without
		// the agent needing to push a fix. Clear is a no-op when no comment
		// exists, so this is safe to run on every green pass.
		r.giveUp.Clear(ctx, session)
		return nil

	case !state.HasPR():
		log.Info("No existing PR, creating from scratch")

	default:
		log.With("state", state).Warn("Unexpected state combination")
	}

	// Record the branch strategy and starting SHAs before any git work. When
	// usePRBranch is false the agent runs on top of the current default branch
	// and the result is force-pushed over the PR branch; on a long-lived PR this
	// is what lets the branch absorb the default branch's history while the PR's
	// base pointer stays frozen at creation. Capturing the decision and the PR
	// head SHA here makes that observable per reconcile instead of only via the
	// GitHub API after the fact.
	branchStrategy := "fresh-from-default-branch"
	if usePRBranch {
		branchStrategy = "iterate-on-pr-branch"
	}
	clog.InfoContext(ctx, "Reconciling PR: leasing clone and running agent",
		"pr", session.PRNumber(),
		"branch_strategy", branchStrategy,
		"pr_head_sha", session.HeadSHA(),
		"commit_count", session.CommitCount(),
		"needs_rebase", state.NeedsRebase(),
		"has_findings", state.HasFindings(),
	)

	// Acquire clone manager for this repo
	cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
	if err != nil {
		return fmt.Errorf("get clone manager: %w", err)
	}

	// PR branch name, used by the base-revalidation probe and the lease below.
	branchName := r.identity + "/" + githubreconciler.PathToBranchSuffix(res.Path)

	// Before iterating on a PR with CI findings, re-check whether the update is
	// still warranted; otherwise a PR for an already-landed or superseded
	// version iterates on build failures forever. An already-landed update
	// closes the PR; a superseded one is refreshed in place with the newest
	// update from the default branch. See WithBaseRevalidation.
	if usePRBranch && r.baseRevalidation {
		closePR, refresh, err := r.needsRefresh(ctx, cloneMgr, session, res, branchName)
		if err != nil {
			return err
		}
		switch {
		case closePR:
			log.Info("Update already landed on the base branch, closing PR")
			r.giveUp.Clear(ctx, session)
			return session.CloseAnyOutstanding(ctx, "Closing this PR: the target update has already landed on the base branch.")
		case refresh:
			log.Info("PR is superseded by a newer update, refreshing it from the default branch")
			usePRBranch = false
		}
	}

	// Lease based on current state:
	// - CI failures on a mergeable PR: lease PR branch for iteration
	// - Otherwise (no PR, needs rebase, or fresh run): lease default branch
	var lease *clonemanager.Lease
	if usePRBranch {
		log.With("branch", branchName).Info("Acquiring clone lease for pull request branch")
		lease, err = cloneMgr.LeaseRef(ctx, res, branchName,
			clonemanager.WithCommitDepth(session.CommitCount()+1))
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

	// Get the worktree for analyzer and request building.
	wt, err := lease.Repo().Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	if r.mode.IsConfig() {
		m, err := loadRepoConfig(wt, r.identity)
		if err != nil {
			return fmt.Errorf("load repo config: %w", err)
		}
		if !m.ShouldFix() {
			log.With("repo_mode", m).Info("Repo config disables fix, skipping")
			return nil
		}
	}

	// Build findings for the agent. On the first pass (no PR or needs rebase),
	// run the analyzer and feed diagnostics. On subsequent passes (CI failures),
	// only feed CI check findings. Mixing the two can cause conflicts (e.g.
	// analyzer suggestions vs protoc codegen expectations).
	var findings []callbacks.Finding
	var diagnostics []Diagnostic
	var allFixed bool
	if usePRBranch {
		// Subsequent pass: only feed CI check findings so the agent focuses
		// on making CI pass without fighting analyzer suggestions.
		findings = session.Findings()
	} else {
		// First pass: run the analyzer. The analyzer may modify files in
		// the worktree to fix some diagnostics, marking them as Fixed.
		// Those modifications persist through createFreshBranch (same-SHA
		// checkout) and are included in the eventual commit.
		diagnostics, err = r.analyzer.Analyze(ctx, wt, []string{res.Path})
		if err != nil {
			return fmt.Errorf("run analyzer: %w", err)
		}
		if len(diagnostics) == 0 {
			log.Info("No diagnostics, closing stale PR if any")
			r.giveUp.Clear(ctx, session)
			return session.CloseAnyOutstanding(ctx, "All diagnostics are resolved.")
		}

		// Split diagnostics: only unfixed ones become agent findings.
		var unfixed []Diagnostic
		for _, d := range diagnostics {
			if !d.Fixed {
				unfixed = append(unfixed, d)
			}
		}
		allFixed = len(unfixed) == 0
		if allFixed {
			log.With("fixed", len(diagnostics)).Info("All diagnostics fixed by analyzer")
		} else {
			findings = make([]callbacks.Finding, 0, len(unfixed))
			for _, d := range unfixed {
				findings = append(findings, d.AsFinding())
			}
		}
	}

	// Build the request for PRData. Even when the analyzer fixed everything,
	// we still build the request so that any stable fields (e.g. SkillsHash)
	// are captured in PRData for change detection.
	request, err := r.buildRequest(ctx, session, wt, findings)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if !allFixed {
		log.With("findings", len(findings)).Info("Running agent")
	}

	// Compute PR labels: static labels + dynamic labels from labelFn.
	labels := slices.Clone(r.labels)
	if r.labelFn != nil {
		labels = append(labels, r.labelFn(ctx, res, diagnostics, findings)...)
	}

	// agentResult captures the agent's output for the no-change path below, where
	// ErrNoChanges short-circuits before Upsert returns it. agentRan guards
	// against surfacing a zero result when the agent never ran (e.g. allFixed).
	var agentResult Resp
	var agentRan bool

	// Upsert PR with changes (analyzer fixes, agent fixes, or both). prData is
	// passed by pointer and the body template renders only after the closure
	// below runs, so fields set post-execution (ReasoningSummary) are visible
	// to the template.
	prData := &PRData[Req]{
		Identity: r.identity,
		Path:     res.Path,
		Request:  request,
	}
	prURL, err := session.Upsert(ctx, prData, false, labels, func(ctx context.Context, branchName string) error {
		// Tee the agent's completed trace so the PR body template can render
		// a per-commit rationale log via {{.ReasoningSummary}} (see
		// ReasoningSummarySnippet): per-action tool-call reasoning when
		// present, falling back to extended-thinking blocks. No-op when the
		// run produced neither.
		ctx, captured := agenttrace.CaptureTrace[Resp](ctx)
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			// If the analyzer already fixed everything, commit its changes
			// directly without invoking the agent. The commit contributes no
			// reasoning entry, but the log persisted from prior iterations
			// must still render rather than drop from the regenerated body,
			// and the title headline must keep anchoring to the PR's primary
			// change rather than reset to the fallback title.
			if allFixed {
				prData.ReasoningSummary = renderReasoningLog(session.ReasoningLog())
				prData.Headline = prHeadline(session.ReasoningLog(), "")
				return commitMessage(diagnostics), nil
			}

			cbs, err := r.buildCallbacks(ctx, session, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			result, err := r.agent.Execute(ctx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			agentResult = result
			agentRan = true

			// Check if the agent left the worktree clean (no actual file changes).
			status, err := wt.Status()
			if err != nil {
				return "", fmt.Errorf("get worktree status: %w", err)
			}
			if status.IsClean() {
				return "", changemanager.ErrNoChanges
			}

			// A commit is certain: log this run's reasoning under the
			// commit's headline and render the accumulated log — prior
			// iterations' entries plus this one — for the PR body. The
			// title headline anchors to the log's first entry so the PR
			// title describes the primary change, not the latest fix-up.
			msg := result.GetCommitMessage()
			session.AppendReasoning(commitHeadline(msg), agenttrace.SummarizeTraceReasoning(captured(), reasoningSummaryMaxChars))
			prData.ReasoningSummary = renderReasoningLog(session.ReasoningLog())
			prData.Headline = prHeadline(session.ReasoningLog(), commitHeadline(msg))

			return msg, nil
		})
	})
	if err != nil {
		if errors.Is(err, changemanager.ErrNoChanges) {
			log.Info("No changes after agent execution, nothing to commit")
			if agentRan {
				r.giveUp.SurfaceResult(ctx, session, agentResult)
			}
			return nil
		}
		return fmt.Errorf("upsert PR: %w", err)
	}

	// The agent pushed a fix: clear any stale give-up comment from a prior
	// iteration where it had nothing to do.
	r.giveUp.Clear(ctx, session)

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}

// needsRefresh re-validates an iterating PR against the current state of the
// repository before iterating:
//   - base no longer wants the change → the update already landed; closePR.
//   - base wants it but the PR branch does not → the PR is on-target; iterate.
//   - both still want it → the PR is stale (a newer version exists); refresh
//     the existing PR with the newest update from the default branch.
func (r *PRReconciler[Req, Resp, CB]) needsRefresh(ctx context.Context, cloneMgr *clonemanager.Manager, session *changemanager.Session[PRData[Req]], res *githubreconciler.Resource, branchName string) (closePR bool, refresh bool, err error) {
	depth := session.CommitCount() + 1

	wantsBase, err := r.revalidate(ctx, cloneMgr, res, res.Ref, depth)
	if err != nil {
		return false, false, fmt.Errorf("revalidate base branch: %w", err)
	}
	if !wantsBase {
		return true, false, nil
	}

	wantsPR, err := r.revalidate(ctx, cloneMgr, res, branchName, depth)
	if err != nil {
		return false, false, fmt.Errorf("revalidate PR branch: %w", err)
	}
	return false, wantsPR, nil
}

// revalidate leases ref, runs the analyzer against its worktree, and reports
// whether the analyzer still wants a change, returning the lease before it
// returns. "Wants a change" means an unfixed diagnostic or a modified worktree:
// monitored packages mutate the worktree and return a Fixed diagnostic, while
// the checker agent returns unfixed diagnostics without mutating it. An
// informational Fixed diagnostic that changes nothing (e.g. image-main-package)
// correctly counts as no change wanted.
func (r *PRReconciler[Req, Resp, CB]) revalidate(ctx context.Context, cloneMgr *clonemanager.Manager, res *githubreconciler.Resource, ref string, depth int) (bool, error) {
	lease, err := cloneMgr.LeaseRef(ctx, res, ref, clonemanager.WithCommitDepth(depth))
	if err != nil {
		return false, fmt.Errorf("acquire lease for %s: %w", ref, err)
	}
	defer func() {
		if err := lease.Return(ctx); err != nil {
			clog.WarnContext(ctx, "Failed to return revalidation lease", "error", err)
		}
	}()

	wt, err := lease.Repo().Worktree()
	if err != nil {
		return false, fmt.Errorf("get worktree: %w", err)
	}
	diags, err := r.analyzer.Analyze(ctx, wt, []string{res.Path})
	if err != nil {
		return false, fmt.Errorf("run analyzer: %w", err)
	}
	for _, d := range diags {
		if !d.Fixed {
			return true, nil
		}
	}
	status, err := wt.Status()
	if err != nil {
		return false, fmt.Errorf("get worktree status: %w", err)
	}
	return !status.IsClean(), nil
}
