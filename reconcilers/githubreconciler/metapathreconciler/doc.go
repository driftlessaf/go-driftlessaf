/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package metapathreconciler provides generic reconcilers for GitHub path
// handlers. Two variants share a common core (mode handling, per-repo
// configuration, and pull request review) and differ in how findings on a
// path are remediated:
//
//   - NewPR runs a metaagent to fix findings and manages the resulting PR
//     through CI feedback loops.
//   - NewIssues files the findings as GitHub issues (via issuemanager) for a
//     downstream bot or human to remediate.
//
// # Pull request reconciliation (both variants)
//
// Pull request resources are handled with a three-way branch:
//
//	(1) skip label → report neutral/skipped status,
//	(2) our identity prefix on branch → report neutral status + re-queue path,
//	(3) other PRs → run analyzer on changed files, report all diagnostics
//	(fixed and unfixed) as check annotations.
//
// # Path reconciliation with NewPR
//
// Path resources trigger analysis: the analyzer runs on the file path,
// diagnostics are converted to findings, and the agent creates/updates a PR.
// Analyzers may modify files in the worktree to fix diagnostics directly,
// marking those diagnostics as Fixed. Only unfixed diagnostics are forwarded
// to the agent as findings. When the analyzer fixes all diagnostics, the
// agent is skipped entirely and the analyzer's changes are committed directly.
//
// # Path reconciliation with NewIssues
//
// Every pass is the same level-triggered re-derivation against the default
// branch: the analyzer runs on the path — seeded with the open issues'
// embedded diagnostics as prior findings, so nondeterministic analyzers
// keep stable keys — its diagnostics are grouped into the desired issue set
// (GroupByRule by default; see Grouping), and issuemanager creates,
// updates, and closes issues to match. There is no PR state machine, no
// push channel — the analyzer must be report-only, and diagnostics marked
// Fixed are dropped — and no fixer: the filed issues carry labels
// (WithLabels / WithLabelFunc) that can arm a downstream remediation bot
// such as the materializer. As with NewPR, the same analyzer reviews pull
// requests in ModeReview.
//
// Deployment notes for NewIssues: reviewing the downstream bot's fix PRs
// (ModeReview) requires the deployment to subscribe to foreign PR events
// (e.g. own_prs_only=false on the terraform module), and the reconciler's
// GitHub identity needs issues:write permission.
//
// # Basic Usage
//
//	// Create the changemanager with your PR templates
//	cm, err := changemanager.New[metapathreconciler.PRData[*MyRequest]](identity, titleTmpl, bodyTmpl)
//
//	// Create the reconciler
//	rec, err := metapathreconciler.NewPR(
//	    ctx,
//	    identity,
//	    analyzer,
//	    cm,
//	    cloneMeta,
//	    agent,
//	    func(ctx context.Context, session *changemanager.Session[metapathreconciler.PRData[*MyRequest]], wt *gogit.Worktree, findings []callbacks.Finding) (*MyRequest, error) {
//	        return &MyRequest{Findings: findings}, nil
//	    },
//	    func(ctx context.Context, session *changemanager.Session[metapathreconciler.PRData[*MyRequest]], lease *clonemanager.Lease) (MyCallbacks, error) {
//	        wt, err := lease.Repo().Worktree()
//	        if err != nil {
//	            return MyCallbacks{}, fmt.Errorf("get worktree: %w", err)
//	        }
//	        return toolcall.NewHistoryTools(
//	            toolcall.NewFindingTools(
//	                toolcall.NewWorktreeTools(toolcall.EmptyTools{}, clonemanager.WorktreeCallbacks(wt)),
//	                session.FindingCallbacks(),
//	            ),
//	            clonemanager.HistoryCallbacks(lease.Repo(), lease.BaseCommit()),
//	        ), nil
//	    },
//	    metapathreconciler.WithLabels("automated"),
//	)
package metapathreconciler
