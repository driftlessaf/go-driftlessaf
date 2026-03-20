/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v84/github"
)

// reconcilePullRequest handles PR events with a three-way branch:
//  1. Skip label present → report neutral/skipped status
//  2. Our identity prefix on branch → report neutral status + re-queue path
//  3. Other PRs → run analyzer on changed files, report findings as check annotations
func (r *Reconciler[Req, Resp, CB]) reconcilePullRequest(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	log := clog.FromContext(ctx)

	// Fetch the PR to get the head branch name and SHA.
	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetch pull request: %w", err)
	}

	// Only process open PRs.
	if pr.GetState() != "open" {
		log.With("state", pr.GetState()).Debug("PR is not open, skipping")
		return nil
	}

	sha := pr.GetHead().GetSHA()
	ctx = clog.WithValues(ctx, "sha", sha)
	log = clog.FromContext(ctx)
	session := r.statusManager.NewSession(gh, res, sha)

	// Check if the status is already at the PR HEAD, completed, and neutral.
	// This lets us skip redundant SetActualState calls in cases 1 and 2.
	currentStatus, err := session.ObservedState(ctx)
	if err != nil {
		return fmt.Errorf("get observed state: %w", err)
	}
	neutralAtHead := currentStatus != nil &&
		currentStatus.ObservedGeneration == sha &&
		currentStatus.Status == "completed" &&
		currentStatus.Conclusion == "neutral"

	// Case 1: Skip label → report neutral/skipped status.
	if hasLabel(pr, fmt.Sprintf("skip:%s", r.identity)) {
		if neutralAtHead {
			log.Debug("Skip status already set for this SHA")
			return nil
		}
		log.Info("PR has skip label, reporting skipped status")
		return session.SetActualState(ctx, "Skipped", &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "neutral",
		})
	}

	// Case 2: Our PR → report neutral status + re-queue the path for processing.
	branch := pr.GetHead().GetRef()
	prefix := r.identity + "/"
	if strings.HasPrefix(branch, prefix) {
		if !neutralAtHead {
			if err := session.SetActualState(ctx, "Managed by "+r.identity, &statusmanager.Status[CheckDetails]{
				Status:     "completed",
				Conclusion: "neutral",
			}); err != nil {
				return fmt.Errorf("set managed status: %w", err)
			}
		}

		path := githubreconciler.BranchSuffixToPath(strings.TrimPrefix(branch, prefix))
		base := pr.GetBase().GetRef()
		pathURL := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", res.Owner, res.Repo, base, path)

		log.With("path", path, "url", pathURL).Info("Re-queuing path from managed PR")
		return workqueue.QueueKeys(workqueue.QueueKey{
			Key:      pathURL,
			Priority: 300, // Highest priority: completing existing PRs is more important than creating new ones.
		})
	}

	// Case 3: Other PR → run analyzer on changed files.
	if !r.mode.ShouldReview() {
		log.Info("Skipping other PR review (fix-only mode)")
		if !neutralAtHead {
			return session.SetActualState(ctx, "Skipped (fix-only)", &statusmanager.Status[CheckDetails]{
				Status:     "completed",
				Conclusion: "neutral",
			})
		}
		return nil
	}

	// Check if we already processed this SHA to avoid redundant work.
	if currentStatus != nil && currentStatus.ObservedGeneration == sha && currentStatus.Status == "completed" {
		log.Debug("Already processed this SHA, skipping")
		return nil
	}

	// Fetch the raw diff once — it provides both the changed file list and
	// the line ranges needed for filtering diagnostics.
	raw, _, err := gh.PullRequests.GetRaw(ctx, res.Owner, res.Repo, res.Number, github.RawOptions{Type: github.Diff})
	if err != nil {
		return fmt.Errorf("get PR diff: %w", err)
	}
	pd, err := parseDiff(raw)
	if err != nil {
		return fmt.Errorf("parse PR diff: %w", err)
	}
	if len(pd.files) == 0 {
		log.Debug("No changed files in PR")
		return session.SetActualState(ctx, "No files to analyze", &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "success",
		})
	}

	// Lease the PR head via GitHub's special pull request ref.
	cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
	if err != nil {
		return fmt.Errorf("get clone manager: %w", err)
	}
	lease, err := cloneMgr.LeaseRef(ctx, res, fmt.Sprintf("refs/pull/%d/head", res.Number))
	if err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}
	defer func() {
		if err := lease.Return(ctx); err != nil {
			log.With("error", err).Warn("Failed to return lease")
		}
	}()

	wt, err := lease.Repo().Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	// Run analyzer on the changed files, then filter diagnostics to only
	// lines touched in the diff.
	diagnostics, err := r.analyzer.Analyze(ctx, wt, pd.files...)
	if err != nil {
		return fmt.Errorf("run analyzer: %w", err)
	}
	diagnostics = filterToChangedLines(diagnostics, pd)

	// Report results via statusmanager.
	if len(diagnostics) == 0 {
		return session.SetActualState(ctx, "No issues found", &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "success",
		})
	}
	return session.SetActualState(ctx, fmt.Sprintf("Found %d issue(s)", len(diagnostics)), &statusmanager.Status[CheckDetails]{
		Status:     "completed",
		Conclusion: "failure",
		Details:    CheckDetails{Diagnostics: diagnostics, Identity: r.identity},
	})
}
