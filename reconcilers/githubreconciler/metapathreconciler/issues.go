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

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/issuemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
)

// defaultCloseMessage is commented on issues closed because their findings
// are no longer reported. Closes happen per issue, not per path, so the
// wording must hold for partial resolution too. See WithCloseMessage.
const defaultCloseMessage = "The findings tracked by this issue are no longer reported."

// IssueReconciler is a reconciler for path handlers that surfaces analyzer
// findings as GitHub issues (via issuemanager) instead of fixing them
// directly. The filed issues carry labels (see WithLabels and WithLabelFunc)
// that can trigger a downstream remediation bot.
type IssueReconciler struct {
	core

	issueManager *issuemanager.IM[IssueData]
	grouping     Grouping

	// closeMessage is commented on issues closed because their findings are
	// no longer reported. See WithCloseMessage.
	closeMessage string
}

// NewIssues creates a path reconciler that reconciles analyzer findings into
// GitHub issues: each pass runs the analyzer, groups its diagnostics into
// the desired issue set (GroupByRule unless overridden via WithGrouping),
// and creates, updates, and closes issues to match (see issuemanager). The
// caller-constructed issue manager carries the issue templates, the per-path
// cap, and any owner/repo overrides.
//
// Unlike NewPR there is no fixer agent: remediation belongs to whoever
// consumes the issues. There is also no push channel, so the analyzer must
// be report-only — worktree modifications are rejected with an error — and
// diagnostics marked Fixed are dropped: a report-only analyzer that sets
// Fixed considers the finding handled, and filing it would create an issue
// no downstream remediation could ever resolve.
//
// Each pass hands the analyzer the diagnostics embedded in the currently
// open issues as its prior findings (see Analyzer), so analyzers with
// nondeterministic output (e.g. agent-based audits) re-confirm tracked
// findings under stable keys rather than re-discovering them under new
// ones.
//
// As with NewPR, the same analyzer reviews pull requests in ModeReview.
func NewIssues(
	ctx context.Context,
	identity string,
	analyzer Analyzer,
	im *issuemanager.IM[IssueData],
	cloneMeta *clonemanager.Meta,
	opts ...IssuesOption,
) (*IssueReconciler, error) {
	switch {
	case analyzer == nil:
		return nil, errors.New("analyzer must be provided")
	case im == nil:
		return nil, errors.New("issue manager must be provided")
	case cloneMeta == nil:
		return nil, errors.New("clone meta must be provided")
	}

	o := issuesOptions{
		commonOptions: commonOptions{mode: ModeConfig},
		closeMessage:  defaultCloseMessage,
		grouping:      GroupByRule,
	}
	for _, opt := range opts {
		opt.applyIssues(&o)
	}

	clog.InfoContext(ctx, "Starting metapathreconciler (issues)", "mode", o.mode)

	c, err := newCore(ctx, identity, analyzer, cloneMeta, o.commonOptions)
	if err != nil {
		return nil, err
	}
	return &IssueReconciler{
		core:         c,
		issueManager: im,
		grouping:     o.grouping,
		closeMessage: o.closeMessage,
	}, nil
}

// Reconcile processes a path or pull request URL.
// For paths: runs the analyzer and reconciles its findings into issues.
// For PRs: reviews other PRs with the same analyzer.
func (r *IssueReconciler) Reconcile(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	switch res.Type {
	case githubreconciler.ResourceTypePath:
		if !r.mode.ShouldFix() && !r.mode.IsConfig() {
			return nil
		}
		return r.reconcilePathIssues(ctx, res, gh)
	case githubreconciler.ResourceTypePullRequest:
		return r.reconcilePullRequest(ctx, res, gh)
	default:
		clog.WarnContext(ctx, "Unexpected resource type", "type", res.Type)
		return nil
	}
}

// reconcilePathIssues handles path resources by running the analyzer against
// the default branch and reconciling its findings into issues. Every pass is
// the same level-triggered re-derivation: there is no PR state machine, no
// iteration pass, and no push — convergence is delegated to issuemanager
// (unchanged findings no-op, changed findings refresh the issue body in
// place, vanished findings close their issues).
func (r *IssueReconciler) reconcilePathIssues(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
	if err != nil {
		return fmt.Errorf("get clone manager: %w", err)
	}
	lease, err := cloneMgr.Lease(ctx, res)
	if err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}
	defer func() {
		if err := lease.Return(ctx); err != nil {
			clog.WarnContext(ctx, "Failed to return lease", "error", err)
		}
	}()

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
			clog.InfoContext(ctx, "Repo config disables fix, skipping", "repo_mode", m)
			return nil
		}
	}

	session, err := r.issueManager.NewSession(ctx, gh, res)
	if err != nil {
		return fmt.Errorf("create issue session: %w", err)
	}

	// Fast path: a deleted path has no findings. Skip the analyzer and
	// reconcile an empty desired set so any outstanding issues are closed.
	if !lease.PathExists() {
		if _, err := session.Reconcile(ctx, nil, nil, r.closeMessage); err != nil {
			return fmt.Errorf("reconcile issues: %w", err)
		}
		clog.InfoContext(ctx, "Path no longer exists, closed any outstanding issues")
		return nil
	}

	// The diagnostics embedded in the currently open issues seed the
	// analyzer as prior findings (see Analyzer), so analyzers with
	// nondeterministic output keep tracked findings under stable keys.
	prior := session.Existing()
	priorDiags := make([]Diagnostic, 0, len(prior))
	for _, p := range prior {
		priorDiags = append(priorDiags, p.Diagnostics...)
	}
	desired, diagnostics, err := deriveIssues(ctx, r.analyzer, r.grouping, res, wt, priorDiags)
	if err != nil {
		return err
	}
	if err := ensureCleanWorktree(wt); err != nil {
		return err
	}

	// Truncate to the session's cap rather than tripping Reconcile's
	// oversized-desired error: a finding-heavy path files what fits now and
	// surfaces the remainder as earlier issues close. Entries already backed
	// by an open issue always survive truncation — only net-new findings are
	// shed — so a still-reported tracked issue is never closed to enforce
	// the cap. When the tracked entries alone exceed the cap (a lowered cap
	// or an inherited backlog), the session is resized to fit them and no
	// new findings are filed until the backlog drains.
	if limit := session.MaxDesired(); limit > 0 && len(desired) > limit {
		truncated := truncateDesired(desired, prior, limit)
		clog.WarnContext(ctx, "Truncating desired issues to session cap",
			"findings", len(desired), "keeping", len(truncated), "cap", limit)
		if len(truncated) > limit {
			session, err = r.issueManager.NewSession(ctx, gh, res,
				issuemanager.WithSessionMaxDesiredIssues(len(truncated)))
			if err != nil {
				return fmt.Errorf("create issue session: %w", err)
			}
		}
		desired = truncated
	}

	labels := slices.Clone(r.labels)
	if r.labelFn != nil {
		labels = append(labels, r.labelFn(ctx, res, diagnostics, nil)...)
	}

	urls, err := session.Reconcile(ctx, desired, labels, r.closeMessage)
	if err != nil {
		return fmt.Errorf("reconcile issues: %w", err)
	}
	clog.InfoContext(ctx, "Issues reconciled", "desired", len(desired), "issues", urls)
	return nil
}

// truncateDesired bounds desired to limit while keeping every entry already
// backed by an open issue: tracked entries are never shed (Reconcile closing
// them would falsely read as "no longer reported"), so the result exceeds
// limit when the tracked entries alone do, and only net-new findings are
// dropped to make room.
func truncateDesired(desired []*IssueData, prior []IssueData, limit int) []*IssueData {
	ordered, tracked := priorFirst(desired, prior)
	return ordered[:max(limit, tracked)]
}

// priorFirst stable-partitions desired so entries matching one of the prior
// open issues (by Equal) precede net-new entries, preserving relative order
// within each partition, and reports how many entries matched.
func priorFirst(desired []*IssueData, prior []IssueData) ([]*IssueData, int) {
	out := make([]*IssueData, 0, len(desired))
	var fresh []*IssueData
	for _, d := range desired {
		if slices.ContainsFunc(prior, d.Equal) {
			out = append(out, d)
		} else {
			fresh = append(fresh, d)
		}
	}
	out = append(out, fresh...)
	return out, len(out) - len(fresh)
}

// ensureCleanWorktree rejects analyzer worktree modifications: nothing in
// issue mode commits or pushes, so a fixing analyzer's changes would be
// silently discarded and its fixed findings never surfaced as issues.
func ensureCleanWorktree(wt *gogit.Worktree) error {
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("get worktree status: %w", err)
	}
	if !status.IsClean() {
		return errors.New("analyzer modified the worktree; issue-mode analyzers must be report-only")
	}
	return nil
}
