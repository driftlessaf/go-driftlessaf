/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package metapathreconciler provides a generic reconciler for metaagent-based
// GitHub path handlers. It orchestrates the common workflow of analyzing a file
// path, running an agent to fix diagnostics, and managing the resulting PR
// through CI feedback loops.
//
// Analyzers may modify files in the worktree to fix diagnostics directly,
// marking those diagnostics as Fixed. Only unfixed diagnostics are forwarded
// to the agent as findings. When the analyzer fixes all diagnostics, the
// agent is skipped entirely and the analyzer's changes are committed directly.
//
// The reconciler handles two resource types:
//
//   - Path resources trigger analysis: the analyzer runs on the file path,
//     diagnostics are converted to findings, and the agent creates/updates a PR.
//     Diagnostics marked as Fixed by the analyzer are excluded from agent findings.
//   - Pull request resources are handled with a three-way branch:
//     (1) skip label → report neutral/skipped status,
//     (2) our identity prefix on branch → report neutral status + re-queue path,
//     (3) other PRs → run analyzer on changed files, report all diagnostics
//     (fixed and unfixed) as check annotations.
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
