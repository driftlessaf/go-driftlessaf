/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
)

// reconcilePullRequest handles PR events with a three-way branch:
//  1. Skip label present → report neutral/skipped status
//  2. Our identity prefix on branch → report neutral status + re-queue path
//  3. Other PRs → run analyzer on changed files, report findings as check annotations
func (r *core) reconcilePullRequest(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
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
	log := clog.FromContext(ctx)
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
	// The branch-name prefix alone does not prove ownership: anyone with push
	// access can open a pull request on a branch that borrows the "<identity>/"
	// prefix, and treating it as ours would attach the managed status and the
	// re-queue (and, downstream, the bot's label, fixer, and signed commits) to a
	// branch the bot never created. Confirm ownership against GitHub before
	// claiming it: the authenticated app must have authored the pull request and
	// its head branch must live in this repository, not a fork. A pull request
	// that only borrows the prefix falls through to Case 3.
	branch := pr.GetHead().GetRef()
	prefix := r.identity + "/"
	if strings.HasPrefix(branch, prefix) {
		owned, err := r.viewerOwnsPR(ctx, gh, res)
		if err != nil {
			return fmt.Errorf("determine pull request ownership: %w", err)
		}
		if owned {
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
		log.With("branch", branch).Info("PR borrows the identity branch prefix but the app did not author it (or it is from a fork); treating as unrelated")
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
		// GitHub refuses to serve a diff over its size limit for as long as the
		// head SHA stands, so retrying cannot help: record a terminal status and
		// complete the key.
		if isDiffTooLarge(err) {
			log.With("error", err).Info("PR diff exceeds GitHub's size limit, reporting neutral status")
			return reportNeutral("Diff too large to analyze")
		}
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

	// filesToAnalyze starts as all changed files; selectReviewFiles narrows
	// it based on mode and exclude_patterns, and returns a terminal status
	// if the PR should be short-circuited (e.g. config says not to review,
	// or all files are excluded).
	var cfg *fullRepoConfig
	if r.mode.IsConfig() {
		loaded, err := loadFullRepoConfig(wt, r.identity)
		if err != nil {
			return fmt.Errorf("load repo config: %w", err)
		}
		cfg = loaded
	} else if r.mode.ShouldReview() {
		loaded, err := loadFullRepoConfig(wt, r.identity)
		if err != nil {
			// If the config file is missing or unreadable, proceed without
			// filtering rather than failing the check entirely.
			clog.WarnContext(ctx, "Failed to load repo config for exclude filtering, proceeding without it", "error", err)
		} else {
			cfg = loaded
		}
	}

	filesToAnalyze, termTitle, term := selectReviewFiles(r.mode, cfg, pd.files)
	if term != nil {
		// currentStatus was already read at the gate above and, having
		// passed the already-processed check, is never a completed status
		// at this SHA — so post directly rather than re-reading via
		// reportNeutral.
		return session.SetActualState(ctx, termTitle, term)
	}

	// Run analyzer on the changed files, then filter diagnostics to only
	// lines touched in the diff.
	diagnostics, err := r.analyzeReview(ctx, wt, filesToAnalyze, raw, pd)
	if err != nil {
		return fmt.Errorf("run analyzer: %w", err)
	}

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

// analyzeReview supplies explicit PR scope through analyzer compositions and
// filters every analyzer's output before it reaches check annotations.
func (r *core) analyzeReview(ctx context.Context, wt *gogit.Worktree, paths []string, raw string, pd *parsedDiff) ([]Diagnostic, error) {
	reviewDiff, err := filterDiffToPaths(raw, paths)
	if err != nil {
		return nil, fmt.Errorf("filter PR review diff: %w", err)
	}
	diagnostics, err := r.analyzer.Analyze(WithReviewDiff(ctx, reviewDiff), wt, paths)
	if err != nil {
		return nil, err
	}
	filtered := slices.DeleteFunc(filterToChangedLines(diagnostics, pd), func(d Diagnostic) bool {
		return !slices.Contains(paths, d.Path)
	})
	if dropped := len(diagnostics) - len(filtered); dropped > 0 {
		var withoutAnchor, outsidePaths int
		for _, d := range diagnostics {
			// Count each suppressed finding once, with missing anchors first.
			if d.Line <= 0 {
				withoutAnchor++
			} else if !slices.Contains(paths, d.Path) {
				outsidePaths++
			}
		}
		clog.InfoContext(ctx, "Dropped PR review diagnostics outside review scope",
			"dropped_count", dropped,
			"without_line_anchor", withoutAnchor,
			"outside_selected_paths", outsidePaths,
			"outside_changed_lines", dropped-withoutAnchor-outsidePaths)
	}
	return filtered, nil
}

// ownsPR is the pure ownership decision for a pull request whose head branch
// carries the reconciler's identity prefix. Ownership requires both that the
// authenticated app authored the pull request (viewerDidAuthor, true only for a
// pull request the app's own installation token opened) and that its head branch
// is not cross-repository (a bot-created branch always lives in the base repo, so
// a fork PR is never ours). Either condition failing means the branch merely
// borrows the prefix.
func ownsPR(viewerDidAuthor, isCrossRepository bool) bool {
	return viewerDidAuthor && !isCrossRepository
}

// viewerOwnsPR reports whether the authenticated app owns this pull request: it
// authored it and its head branch is not from a fork. The branch name is
// attacker-controllable, so ownership is confirmed against GitHub. Both fields
// are GraphQL-only, so this issues one extra query, reached only for a pull
// request whose branch already matches the identity prefix (rare on a busy
// repository), not for every event.
func (r *core) viewerOwnsPR(ctx context.Context, gh *github.Client, res *githubreconciler.Resource) (bool, error) {
	number, err := gqlPRNumber(res.Number)
	if err != nil {
		return false, err
	}
	var q struct {
		Repository struct {
			PullRequest *struct {
				ViewerDidAuthor   bool
				IsCrossRepository bool
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	gql := graphqlclient.NewGraphQLClient(gh)
	if err := gql.Query(ctx, "MetaPathReconcilerPROwnership", &q, map[string]any{
		"owner":  githubv4.String(res.Owner),
		"repo":   githubv4.String(res.Repo),
		"number": number,
	}); err != nil {
		return false, fmt.Errorf("querying pull request ownership: %w", err)
	}
	prq := q.Repository.PullRequest
	if prq == nil {
		return false, nil
	}
	return ownsPR(prq.ViewerDidAuthor, prq.IsCrossRepository), nil
}

// gqlPRNumber converts a pull request number to the GraphQL Int scalar. GitHub
// numbers fit int32; the bound check keeps the conversion from wrapping on a
// corrupt resource.
func gqlPRNumber(n int) (githubv4.Int, error) {
	if n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("pull request number %d outside int32", n)
	}
	return githubv4.Int(n), nil
}

// selectReviewFiles decides which files to analyze for a PR review and whether
// to short-circuit with a terminal status. It is a pure function with no I/O,
// extracted from reconcilePullRequest so it can be unit-tested without the full
// reconcile harness (worktree, GitHub client, etc.).
//
// Parameters:
//   - mode: the reconciler's operating mode.
//   - cfg: the repo config loaded from .{identity}.yaml, or nil if the file
//     could not be loaded (treated as fail-open in ShouldReview mode, and as
//     "not configured for review" in IsConfig mode).
//   - files: the changed files in the PR diff.
//
// Returns:
//   - keep: the files to pass to the analyzer (subset of files after filtering).
//   - title: the check-run title to use when term != nil.
//   - term: if non-nil, the caller should short-circuit with this status instead
//     of running the analyzer.
func selectReviewFiles(mode Mode, cfg *fullRepoConfig, files []string) (keep []string, title string, term *statusmanager.Status[CheckDetails]) {
	if mode.IsConfig() {
		// In config mode the repo's .{identity}.yaml determines whether to
		// review. A missing or unconfigured file (cfg == nil or
		// cfg.Mode.ShouldReview() == false) means "not configured for review".
		if cfg == nil || !cfg.Mode.ShouldReview() {
			return nil, "Skipped (config)", &statusmanager.Status[CheckDetails]{
				Status:     "completed",
				Conclusion: "neutral",
			}
		}
		// Config says to review: apply exclude_patterns filtering.
		files = applyExcludeFilter(files, cfg)
	} else if mode.ShouldReview() {
		// Non-config review modes (ModeReview, ModeAll): apply
		// exclude_patterns if a config was loaded. A nil cfg means the load
		// failed; fail-open by proceeding without filtering.
		if cfg != nil {
			files = applyExcludeFilter(files, cfg)
		}
	}

	if len(files) == 0 {
		return nil, "No files to analyze", &statusmanager.Status[CheckDetails]{
			Status:     "completed",
			Conclusion: "neutral",
		}
	}
	return files, "", nil
}

// isDiffTooLarge reports whether err is GitHub declining to serve a pull
// request diff because it exceeds the API's size limit: an HTTP 406 response,
// or an error entry with the too_large code.
func isDiffTooLarge(err error) bool {
	ghErr, ok := errors.AsType[*github.ErrorResponse](err)
	if !ok {
		return false
	}
	if ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotAcceptable {
		return true
	}
	return slices.ContainsFunc(ghErr.Errors, func(e github.Error) bool { return e.Code == "too_large" })
}
