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

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v84/github"
)

// reconcilePath handles path resources by running the analyzer and agent.
func (r *Reconciler[Req, Resp, CB]) reconcilePath(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
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
		clog.InfoContext(ctx, "PR hit turn limit")
		_, err := session.ApplyTurnLimit(ctx)
		return err

	// Historically we delayed here (commented code below), but in high-volume
	// repositories github can take a long time to compute mergeability, so we
	// are choosing to optimistically proceed as-if there isn't a rebase needed
	// when github has not computed mergeability.
	// case state.IsUnknown():
	// 	log.Info("PR merge status unknown, requeuing to check again shortly")
	// 	return workqueue.RequeueAfter(2 * time.Minute)

	case state.HasFindings():
		log.With("findings", len(session.Findings())).Info("PR has CI findings, iterating")
		usePRBranch = true

	case state.HasPendingChecks():
		log.With("pending_checks", session.PendingChecks()).Info("PR has pending checks, skipping")
		return nil

	case state.HasNoConflicts():
		log.Info("PR is green, leaving it for human review")
		if _, err := session.ApplyReadyForReview(ctx); err != nil {
			return fmt.Errorf("apply ready-for-review: %w", err)
		}
		return nil

	case !state.HasPR():
		log.Info("No existing PR, creating from scratch")

	default:
		log.With("state", state).Warn("Unexpected state combination")
	}

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
		diagnostics, err = r.analyzer.Analyze(ctx, wt, res.Path)
		if err != nil {
			return fmt.Errorf("run analyzer: %w", err)
		}
		if len(diagnostics) == 0 {
			log.Info("No diagnostics, closing stale PR if any")
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
	labels := slices.Clone(r.prLabels)
	if r.labelFn != nil {
		labels = append(labels, r.labelFn(ctx, res, diagnostics, findings)...)
	}

	// Upsert PR with changes (analyzer fixes, agent fixes, or both).
	prURL, err := session.Upsert(ctx, &PRData[Req]{
		Identity: r.identity,
		Path:     res.Path,
		Request:  request,
	}, false, labels, func(ctx context.Context, branchName string) error {
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			// If the analyzer already fixed everything, commit its
			// changes directly without invoking the agent.
			if allFixed {
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

			// Check if the agent left the worktree clean (no actual file changes).
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
			return nil
		}
		return fmt.Errorf("upsert PR: %w", err)
	}

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}

// needsRefresh re-validates an iterating PR against the current state of the
// repository before iterating:
//   - base no longer wants the change → the update already landed; closePR.
//   - base wants it but the PR branch does not → the PR is on-target; iterate.
//   - both still want it → the PR is stale (a newer version exists); refresh
//     the existing PR with the newest update from the default branch.
func (r *Reconciler[Req, Resp, CB]) needsRefresh(ctx context.Context, cloneMgr *clonemanager.Manager, session *changemanager.Session[PRData[Req]], res *githubreconciler.Resource, branchName string) (closePR bool, refresh bool, err error) {
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
func (r *Reconciler[Req, Resp, CB]) revalidate(ctx context.Context, cloneMgr *clonemanager.Manager, res *githubreconciler.Resource, ref string, depth int) (bool, error) {
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
	diags, err := r.analyzer.Analyze(ctx, wt, res.Path)
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
