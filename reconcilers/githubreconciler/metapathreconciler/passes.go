/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
)

// passEntry is one reasoning-log entry produced by an agent pass. Entries are
// collected during the passes and appended to the session only once a commit is
// certain, so a run that changes nothing leaves the log untouched.
type passEntry struct {
	headline string
	summary  string
}

// passesOutcome carries what runAgentPasses produced: the single combined commit
// message, one reasoning-log entry per completed pass, and the aggregated
// no-change explanation across passes (empty when at least one pass changed
// files or none explained a give-up).
type passesOutcome struct {
	commitMessage     string
	entries           []passEntry
	giveUpExplanation string
}

// runAgentPasses runs the agent over the findings, optionally split into
// fan-out groups (one focused pass per group) with a local-verification loop
// after each pass. Every pass edits the same checkout, so all edits accumulate
// into the single commit. It mutates neither the session nor prData beyond
// restoring the request's full finding set on return; the caller applies the
// entries once it confirms a commit.
func (r *PRReconciler[Req, Resp, CB]) runAgentPasses(ctx context.Context, wt *gogit.Worktree, cbs CB, request Req, findings []callbacks.Finding) (passesOutcome, error) {
	groups := groupsForRequest(any(request), findings)
	fo, hasFanOut := any(request).(fanOut)
	withinBudget := budgetChecker(ctx, any(request))

	results := make([]Resp, 0, len(groups))
	entries := make([]passEntry, 0, len(groups))
	explanations := make([]string, 0, len(groups))

	for _, g := range groups {
		// Stop starting new groups once the wall-clock budget is spent, so the
		// groups already applied are committed instead of discarded on a hard
		// deadline. Always run at least the first group.
		if len(results) > 0 && !withinBudget() {
			clog.InfoContext(ctx, "reconcile budget spent, committing completed fan-out groups", "completed", len(results), "total", len(groups))
			break
		}
		if hasFanOut {
			fo.SetLocalFindings(g.Findings)
		}
		result, summary, err := r.executeCaptured(ctx, request, cbs)
		if err != nil {
			return passesOutcome{}, fmt.Errorf("execute agent: %w", err)
		}
		result, verifySummaries, remaining, err := r.verifyPass(ctx, wt, cbs, request, result, withinBudget)
		if err != nil {
			return passesOutcome{}, err
		}
		results = append(results, result)
		explanations = append(explanations, noChangeExplanation(result))

		summary = joinSummaries(append([]string{summary}, verifySummaries...))
		if len(remaining) > 0 {
			summary = appendRemainingNote(summary, remaining)
		}
		// The persisted headline never carries the group key: it anchors the PR
		// title, so a "family: subject" headline would promote the family label
		// into the title on the next iteration. Record the family in the body
		// instead.
		if len(groups) > 1 && g.Key != "" {
			summary = "Family " + g.Key + ":\n" + summary
		}
		entries = append(entries, passEntry{headline: commitHeadline(result.GetCommitMessage()), summary: summary})
	}

	// Restore the full finding set the reconcile set out to address: each pass
	// and each verification round narrowed request.Findings via SetLocalFindings,
	// and the PR body renders that field, so without this the body would list
	// only the last pass's findings rather than every finding addressed.
	if setter, ok := any(request).(interface{ SetLocalFindings([]callbacks.Finding) }); ok {
		setter.SetLocalFindings(findings)
	}

	msg := combineCommitMessages(groups, results)
	// Run the post-edit finalizer once, over every module the passes touched,
	// rather than after each pass, and carry its note into the commit body.
	if fin, ok := any(request).(finalizer); ok && len(results) > 0 {
		if note := fin.Finalize(ctx, wt); note != "" {
			msg = appendCommitNote(msg, note)
		}
	}
	return passesOutcome{commitMessage: msg, entries: entries, giveUpExplanation: aggregateExplanations(explanations)}, nil
}

// executeCaptured runs one agent execution under a fresh trace-capture context
// and returns the result together with a concise reasoning summary for that
// single execution. Capturing per execution — rather than once around a whole
// pass — keeps each execution's rationale instead of letting a later re-run
// overwrite it (CaptureTrace keeps only the most recent trace).
func (r *PRReconciler[Req, Resp, CB]) executeCaptured(ctx context.Context, request Req, cbs CB) (Resp, string, error) {
	cctx, captured := agenttrace.CaptureTrace[Resp](ctx)
	result, err := r.agent.Execute(cctx, request, cbs)
	if err != nil {
		return result, "", err
	}
	return result, agenttrace.SummarizeTraceReasoning(captured(), reasoningSummaryMaxChars), nil
}

// verifyPass runs the request's local verification loop after an agent pass:
// while the checkout reports findings, it feeds them back and re-runs the agent,
// up to the request's MaxLocalRounds, stopping early once the wall-clock budget
// is spent. A request that does not opt in returns its input unchanged. It
// returns the final result, each round's reasoning summary, any findings still
// open after the cap, and any re-run error (which the caller propagates so a
// failed re-run's partial edits are never committed).
func (r *PRReconciler[Req, Resp, CB]) verifyPass(ctx context.Context, wt *gogit.Worktree, cbs CB, request Req, initial Resp, withinBudget func() bool) (Resp, []string, []callbacks.Finding, error) {
	lv, ok := any(request).(localVerifier)
	if !ok || lv.MaxLocalRounds() <= 0 {
		return initial, nil, nil, nil
	}
	result, rounds, summaries, remaining, err := runVerification(
		func() []callbacks.Finding { return lv.LocalVerify(ctx, wt) },
		func(f []callbacks.Finding) { lv.SetLocalFindings(f) },
		func() (Resp, string, error) { return r.executeCaptured(ctx, request, cbs) },
		lv.MaxLocalRounds(),
		initial,
		withinBudget,
	)
	if err != nil {
		return result, summaries, remaining, fmt.Errorf("local verification re-run: %w", err)
	}
	if len(remaining) > 0 {
		clog.InfoContext(ctx, "local verification findings remain after the cap", "rounds", rounds, "remaining", len(remaining))
	}
	return result, summaries, remaining, nil
}

// budgetChecker returns a predicate reporting whether enough wall-clock time
// remains to start more work. A request that opts into reconcileBudget under a
// context with a deadline is bounded to leave at least its margin; anything else
// is unbounded (the predicate always reports true).
func budgetChecker(ctx context.Context, request any) func() bool {
	b, ok := request.(reconcileBudget)
	if !ok {
		return func() bool { return true }
	}
	margin := b.ReconcileMargin()
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline || margin <= 0 {
		return func() bool { return true }
	}
	return func() bool { return time.Until(deadline) > margin }
}

// combineCommitMessages builds the single commit message for the passes. A lone
// pass keeps its message verbatim, so a run without fan-out is byte-for-byte what
// it was before. Multiple passes take the first pass's headline and list every
// family beneath it, so the commit records what each pass did.
func combineCommitMessages[Resp interface{ GetCommitMessage() string }](groups []FanOutGroup, results []Resp) string {
	if len(results) == 1 {
		return results[0].GetCommitMessage()
	}
	var b strings.Builder
	b.WriteString(commitHeadline(results[0].GetCommitMessage()))
	b.WriteString("\n\nApplied by per-family fixer passes:\n")
	for i, g := range groups {
		if i >= len(results) {
			break
		}
		key := g.Key
		if key == "" {
			key = "findings"
		}
		fmt.Fprintf(&b, "- %s: %s\n", key, commitHeadline(results[i].GetCommitMessage()))
	}
	return b.String()
}

// appendCommitNote appends a note block to a commit message, keeping the first
// line (the subject) intact, so a post-edit tooling note survives into the
// commit and the pull request.
func appendCommitNote(message, note string) string {
	message = strings.TrimRight(message, "\n")
	if message == "" {
		return note
	}
	return message + "\n\n" + note
}

// appendRemainingNote records that local checks still report findings after the
// verification cap, so the reasoning log and PR body show they were attempted.
func appendRemainingNote(summary string, remaining []callbacks.Finding) string {
	note := fmt.Sprintf("Local checks still report %d finding(s) after the verification cap; CI reports them on the pull request.", len(remaining))
	if summary == "" {
		return note
	}
	return summary + "\n\n" + note
}

// joinSummaries joins the non-empty reasoning summaries of a pass — its initial
// execution and each verification round — into one block, so the reasoning log
// reflects the whole pass rather than only its last re-run.
func joinSummaries(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// noChangeExplanation returns the deliberate-no-op explanation a result carries,
// or "" when it carries none. A typed-nil pointer still satisfies Explainer, so
// guard against it before calling the accessor.
func noChangeExplanation(result any) string {
	if rv := reflect.ValueOf(result); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return ""
	}
	ex, ok := result.(changemanager.Explainer)
	if !ok {
		return ""
	}
	return ex.GetNoChangeExplanation()
}

// aggregateExplanations joins the distinct, non-empty give-up explanations across
// passes, so a no-change reconcile that fanned out surfaces every family's reason
// rather than only the last pass's.
func aggregateExplanations(explanations []string) string {
	seen := make(map[string]struct{}, len(explanations))
	kept := make([]string, 0, len(explanations))
	for _, e := range explanations {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		kept = append(kept, e)
	}
	return strings.Join(kept, "\n\n")
}
