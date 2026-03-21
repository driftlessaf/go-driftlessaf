/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
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
		log.Info("PR should be skipped, not updating")
		return nil

	// If the PR is not mergeable, ignore everything about the existing PR
	// and start from scratch on the default branch.
	case state.NeedsRebase():
		log.Info("PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		log.Info("PR hit turn limit")
		_, err := session.ApplyTurnLimit(ctx)
		return err

	case state.IsUnknown():
		log.Info("PR merge status unknown, requeuing to check again shortly")
		return workqueue.RequeueAfter(2 * time.Minute)

	case state.HasFindings():
		log.With("findings", len(session.Findings())).Info("PR has CI findings, iterating")
		usePRBranch = true

	case state.HasPendingChecks():
		log.With("pending_checks", session.PendingChecks()).Info("PR has pending checks, skipping")
		return nil

	case state.HasNoConflicts():
		log.Info("PR is green, leaving it for human review")
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

	// Lease based on current state:
	// - CI failures on a mergeable PR: lease PR branch for iteration
	// - Otherwise (no PR, needs rebase, or fresh run): lease default branch
	var lease *clonemanager.Lease
	if usePRBranch {
		branchName := r.identity + "/" + githubreconciler.PathToBranchSuffix(res.Path)
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

	// Build findings for the agent. On the first pass (no PR or needs rebase),
	// run the analyzer and feed diagnostics. On subsequent passes (CI failures),
	// only feed CI check findings. Mixing the two can cause conflicts (e.g.
	// analyzer suggestions vs protoc codegen expectations).
	var findings []callbacks.Finding
	if usePRBranch {
		// Subsequent pass: only feed CI check findings so the agent focuses
		// on making CI pass without fighting analyzer suggestions.
		findings = session.Findings()
	} else {
		// First pass: run the analyzer and feed diagnostics.
		wt, err := lease.Repo().Worktree()
		if err != nil {
			return fmt.Errorf("get worktree: %w", err)
		}
		diagnostics, err := r.analyzer.Analyze(ctx, wt, res.Path)
		if err != nil {
			return fmt.Errorf("run analyzer: %w", err)
		}
		if len(diagnostics) == 0 {
			log.Info("No diagnostics, closing stale PR if any")
			return session.CloseAnyOutstanding(ctx, "All diagnostics are resolved.")
		}
		findings = make([]callbacks.Finding, 0, len(diagnostics))
		for _, d := range diagnostics {
			findings = append(findings, d.AsFinding())
		}
	}

	log.With("findings", len(findings)).Info("Running agent")

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, findings)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// Upsert PR with agent changes
	prURL, err := session.Upsert(ctx, &PRData[Req]{
		Identity: r.identity,
		Path:     res.Path,
		Request:  request,
	}, false, r.prLabels, func(ctx context.Context, branchName string) error {
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, _ *gogit.Worktree) (string, error) {
			cbs, err := r.buildCallbacks(ctx, session, lease)
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

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}
