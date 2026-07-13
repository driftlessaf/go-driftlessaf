/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"slices"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
)

// SubmitGate returns a result validator that runs the given reviewer over
// the files the agent changed before its submission is accepted. Register it
// on the agent's executor (metaagent.Config.ResultValidators or the
// executors' WithResultValidator option); when the agent runs under a clone
// lease (Lease.MakeAndPushChanges carries the worktree on the update
// context), each submission re-reviews the worktree and diagnostics are
// rejected back to the model as findings so it addresses them and resubmits.
// Outside a lease — unit tests, ad-hoc runs — the gate accepts silently.
//
// The reviewer implements Analyzer, the same interface the reconcilers use
// to seed agent requests and to leave feedback on pull requests, so an
// existing reviewer (e.g. skillup's) can gate its own fixer or any other
// reconciler's agent unchanged. The executors evaluate a submission only
// after the turn's other tool handlers have completed (see
// callbacks.ResultValidator), so the gate reads a quiesced worktree rather
// than racing the agent's own writes. The reviewer itself MUST be
// report-only: validators run in parallel with each other, and the leased
// go-git worktree is not safe for concurrent use — for the same reason, run
// several reviewers by composing them sequentially inside one Analyzer
// rather than registering several gates. Diagnostics a reviewer marks Fixed
// never block, and are logged as report-only-contract violations rather
// than silently trusted.
//
// The scope the reviewer sees is the set of paths changed in the worktree
// relative to the leased commit — staged or unstaged, tracked or untracked,
// excluding deletions — mirroring how pull-request review scopes to changed
// files. An agent that changed nothing has nothing to review and passes.
// The gate reviews disk state while Lease.MakeAndPushChanges commits the
// index, so "what was reviewed is what ships" assumes writes are staged as
// they land — which every clonemanager.WorktreeCallbacks operation and every
// in-tree fixing analyzer does; a tool handler that writes without staging
// reopens that gap.
//
// The gate is stateless: every diagnostic reported blocks, every evaluation
// is independent, and nothing is passed as Analyze's prior. A
// nondeterministic reviewer (an agent-based audit) may therefore report
// findings on one evaluation that it did not report on the last; the
// executor's turn limit is the backstop against a reviewer that never runs
// dry.
func SubmitGate[Resp any](reviewer Analyzer) callbacks.ResultValidator[Resp] {
	return func(ctx context.Context, _ Resp, _ string) ([]callbacks.Finding, error) {
		wt, ok := clonemanager.WorktreeFromContext(ctx)
		if !ok {
			return nil, nil
		}
		paths, err := changedPaths(wt)
		if err != nil {
			return nil, fmt.Errorf("computing changed paths: %w", err)
		}
		if len(paths) == 0 {
			return nil, nil
		}

		diags, err := reviewer.Analyze(ctx, wt, paths)
		if err != nil {
			// The error aborts the primary agent's run (the ResultValidator
			// contract), so name the reviewer by its concrete type to keep
			// the failure diagnosable from an incident.
			return nil, fmt.Errorf("reviewer %T: %w", reviewer, err)
		}

		var findings []callbacks.Finding
		for _, d := range diags {
			if d.Fixed {
				// Fixed is the only self-reported signal that a reviewer
				// violated the report-only contract (issue mode's clean-
				// worktree check has no submit-time analogue — the worktree
				// is legitimately dirty with the agent's changes), so
				// surface it: a fixing reviewer's worktree mutations ship in
				// the agent's commit with no other attribution.
				clog.WarnContext(ctx, "Submit gate ignored a Fixed diagnostic; a fixing reviewer's worktree mutations ship in the agent's commit",
					"reviewer", fmt.Sprintf("%T", reviewer),
					"rule", d.Rule,
					"path", d.Path)
				continue
			}
			findings = append(findings, d.AsFinding())
		}
		if len(findings) > 0 {
			// The executor already counts rejections (the
			// submit_result_rejected tool-call metric); this names the gate
			// as the cause so a run burning turns is attributable to its
			// reviewer from the logs.
			clog.InfoContext(ctx, "Submit gate rejected submission",
				"reviewer", fmt.Sprintf("%T", reviewer),
				"findings", len(findings),
				"changed_paths", len(paths))
		}
		return findings, nil
	}
}

// changedPaths lists the repo-relative paths that differ from the leased
// commit — staged or unstaged, tracked or untracked — excluding deletions,
// which leave no content to review. Paths are sorted so reviewers see a
// deterministic scope.
func changedPaths(wt *gogit.Worktree) ([]string, error) {
	status, err := wt.Status()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(status))
	for path, s := range status {
		if s.Staging == gogit.Unmodified && s.Worktree == gogit.Unmodified {
			continue
		}
		if s.Staging == gogit.Deleted || s.Worktree == gogit.Deleted {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}
