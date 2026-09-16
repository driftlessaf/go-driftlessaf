/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
)

// FanOutGroup is one partition of a pass's findings that a dedicated agent run
// handles. Splitting findings by a stable key and giving each group its own run
// keeps a focused pass (for example one finding class) from being diluted
// by unrelated findings, while every group's edits accumulate into the single
// commit.
type FanOutGroup struct {
	// Key is a stable, human-readable identity for the group (for example
	// "skill/family"). It labels the group in the run log.
	Key string
	// Findings are the findings this group's run receives.
	Findings []callbacks.Finding
}

// fanOut is an optional request capability. When a request implements it, the
// reconciler runs one agent pass per returned group, configuring each pass with
// SetLocalFindings before it runs. A request that does not implement it, or that
// returns fewer than two groups, runs once exactly as before — so every consumer
// that does not opt in is unaffected.
type fanOut interface {
	FanOutGroups() []FanOutGroup
	SetLocalFindings([]callbacks.Finding)
}

// localVerifier is an optional request capability. When a request implements it,
// the reconciler runs LocalVerify over the checkout after an agent pass; a
// non-empty result is applied back to the request (SetLocalFindings) and the
// agent runs again, up to MaxLocalRounds times, so a change opens already clean
// instead of relying on a later CI round. A request that does not implement it is
// unaffected.
type localVerifier interface {
	LocalVerify(ctx context.Context, wt *gogit.Worktree) []callbacks.Finding
	SetLocalFindings([]callbacks.Finding)
	MaxLocalRounds() int
}

// finalizer is an optional request capability. When a request implements it, the
// reconciler runs Finalize once over the checkout after every pass and before the
// commit — a single normalization of every module the passes touched, rather than
// one per pass — and folds the returned note into the commit body. A request that
// does not implement it is unaffected.
type finalizer interface {
	Finalize(ctx context.Context, wt *gogit.Worktree) string
}

// reconcileBudget is an optional request capability: the wall-clock margin the
// reconciler leaves before its context deadline. The reconciler stops starting
// new fan-out groups and verification rounds once less than the margin remains,
// so work already applied is committed rather than discarded when the deadline
// (for example a Cloud Run request timeout) fires. A request that does not
// implement it, or a context with no deadline, is unbounded as before.
type reconcileBudget interface {
	ReconcileMargin() time.Duration
}

// groupsForRequest returns the fan-out groups a request asks for, or a single
// group carrying all findings when the request does not opt into fan-out (or
// asks for none). The single-group result reproduces the pre-fan-out behavior
// exactly.
func groupsForRequest(request any, findings []callbacks.Finding) []FanOutGroup {
	if fo, ok := request.(fanOut); ok {
		if groups := fo.FanOutGroups(); len(groups) > 0 {
			return groups
		}
	}
	return []FanOutGroup{{Findings: findings}}
}

// runVerification re-runs run while verify reports findings, up to maxRounds. It
// applies each round's findings (apply) before re-running, and returns the final
// result, the number of extra rounds it drove, each round's reasoning summary,
// any findings still present after the cap, and the first run error.
//
// A run error aborts the loop and is returned rather than swallowed: the failed
// re-run has already written partial edits through the checkout, so continuing
// would commit them. Returning it lets the caller abort before any commit. The
// loop also stops starting a new round once withinBudget reports the wall-clock
// budget is spent, so work already applied is preserved.
func runVerification[Resp any](
	verify func() []callbacks.Finding,
	apply func([]callbacks.Finding),
	run func() (Resp, string, error),
	maxRounds int,
	initial Resp,
	withinBudget func() bool,
) (result Resp, rounds int, summaries []string, remaining []callbacks.Finding, err error) {
	result = initial
	for rounds < maxRounds {
		if withinBudget != nil && !withinBudget() {
			return result, rounds, summaries, nil, nil
		}
		findings := verify()
		if len(findings) == 0 {
			return result, rounds, summaries, nil, nil
		}
		apply(findings)
		r, summary, runErr := run()
		rounds++
		if runErr != nil {
			return result, rounds, summaries, findings, runErr
		}
		result = r
		if summary != "" {
			summaries = append(summaries, summary)
		}
	}
	return result, rounds, summaries, verify(), nil
}

// runFanOut runs runGroup once per group, in order, applying each group's
// findings first. It returns the per-group results. A group error stops the
// sequence and is returned with the results gathered so far.
func runFanOut[Resp any](
	groups []FanOutGroup,
	apply func([]callbacks.Finding),
	runGroup func(FanOutGroup) (Resp, error),
) ([]Resp, error) {
	results := make([]Resp, 0, len(groups))
	for _, g := range groups {
		apply(g.Findings)
		r, err := runGroup(g)
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}
