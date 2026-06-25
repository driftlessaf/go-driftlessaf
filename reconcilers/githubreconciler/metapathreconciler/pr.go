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
	"github.com/google/go-github/v88/github"
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
		clog.DebugContext(ctx, "PR is not open, skipping", "state", pr.GetState())
		return nil
	}

	sha := pr.GetHead().GetSHA()
	ctx = clog.WithValues(ctx, "sha", sha)
	log = clog.FromContext(ctx)
	session := r.statusManager.NewSession(gh, res, sha)

	// reportNeutral posts a completed/neutral status for this SHA, but only
	// after confirming we have not already done so. ObservedState issues a
	// check-runs API request, so it is deferred into here: the cases that
	// ignore a PR outright (the common case on a busy repository) cost only
	// the single PR fetch above, not an extra check-runs read per event.
	reportNeutral := func(title string) error {
		current, err := session.ObservedState(ctx)
		if err != nil {
			return fmt.Errorf("get observed state: %w", err)
		}
		if current != nil &&
			current.ObservedGeneration == sha &&
			current.Status == "completed" &&
			current.Conclusion == "neutral" {
			clog.DebugContext(ctx, "Neutral status already set for this SHA")
			return nil
		}
		return session.SetActualState(ctx, title, &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "neutral",
		})
	}

	// Case 1: Skip label → report neutral/skipped status.
	if hasLabel(pr, fmt.Sprintf("skip:%s", r.identity)) {
		clog.InfoContext(ctx, "PR has skip label, reporting skipped status")
		return reportNeutral("Skipped")
	}

	// Case 2: Our PR → report neutral status + re-queue the path for processing.
	branch := pr.GetHead().GetRef()
	prefix := r.identity + "/"
	if strings.HasPrefix(branch, prefix) {
		if err := reportNeutral("Managed by " + r.identity); err != nil {
			return fmt.Errorf("set managed status: %w", err)
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

	// Case 3: Other PR. In fix-only mode there is nothing to do for a PR that
	// isn't ours, so return immediately — skipping the check-runs read and
	// status write the cases above perform. This is the dominant case on a
	// busy repository, so keeping it to the single PR fetch above bounds the
	// reconciler's GitHub API load to managed PRs rather than total PR volume.
	if !r.mode.ShouldReview() && !r.mode.IsConfig() {
		clog.DebugContext(ctx, "Unrelated PR in fix-only mode, skipping")
		return nil
	}

	// Review/config mode: we will run the analyzer. Read the observed state to
	// avoid re-processing a SHA we have already reported on.
	currentStatus, err := session.ObservedState(ctx)
	if err != nil {
		return fmt.Errorf("get observed state: %w", err)
	}
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

	// filesToAnalyze starts as all changed files; config mode may narrow it,
	// and all review modes apply exclude_patterns filtering.
	filesToAnalyze := pd.files

	if r.mode.IsConfig() {
		cfg, err := loadFullRepoConfig(wt, r.identity)
		if err != nil {
			return fmt.Errorf("load repo config: %w", err)
		}
		if !cfg.Mode.ShouldReview() {
			// currentStatus was already read at the gate above and, having
			// passed the already-processed check, is never a completed status
			// at this SHA — so post directly rather than re-reading via
			// reportNeutral.
			return session.SetActualState(ctx, "Skipped (config)", &statusmanager.Status[CheckDetails]{
				Status:     "completed",
				Conclusion: "neutral",
			})
		}
		// Filter out files that match exclude_patterns (e.g. testdata
		// fixtures). We intentionally do NOT apply path_patterns here:
		// path_patterns are trigger keys for the fix/resync path (e.g.
		// "go.mod" represents a module root whose entire tree is analyzed),
		// not a scope restriction for PR review. Applying them would silently
		// drop review feedback for any changed file that does not literally
		// match a trigger key.
		filesToAnalyze = applyExcludeFilter(filesToAnalyze, cfg)
	} else if r.mode.ShouldReview() {
		// Non-config review modes (ModeReview, ModeAll) also apply
		// exclude_patterns from the repo config file so that deliberately-
		// broken fixtures under testdata/ (and other excluded paths) are not
		// flagged in PR review, matching the behaviour of the fix/resync paths.
		cfg, err := loadFullRepoConfig(wt, r.identity)
		if err != nil {
			// If the config file is missing or unreadable, proceed without
			// filtering rather than failing the check entirely.
			clog.WarnContext(ctx, "Failed to load repo config for exclude filtering, proceeding without it", "error", err)
		} else {
			filesToAnalyze = applyExcludeFilter(filesToAnalyze, cfg)
		}
	}

	if len(filesToAnalyze) == 0 {
		clog.DebugContext(ctx, "No files to analyze after path filtering")
		return session.SetActualState(ctx, "No files to analyze", &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "success",
		})
	}

	// Run analyzer on the changed files, then filter diagnostics to only
	// lines touched in the diff.
	diagnostics, err := r.analyzer.Analyze(ctx, wt, filesToAnalyze...)
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
