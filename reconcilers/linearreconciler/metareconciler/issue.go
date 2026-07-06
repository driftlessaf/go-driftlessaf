/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
)

// Linear workflow state types. See
// https://developers.linear.app/docs/graphql/working-with-the-graphql-api/workflow-states
//
// IMPORTANT: Linear uses American spelling — "canceled" (one L), NOT
// "cancelled" (two L's). Both the "Cancelled" and "Duplicate" UI states
// map to type=canceled. The "Done" UI state maps to type=completed.
//
// Exported so consumers (e.g. bot SaveCallbacks reacting to terminal
// state transitions) can gate on the canonical Linear type without
// copying magic strings.
const (
	StateTypeCompleted = "completed"
	StateTypeCanceled  = "canceled"
)

// reconcileIssue resolves the repo target from the Linear issue, then runs the
// agent to create/update a GitHub PR. The state machine mirrors
// githubreconciler/metareconciler/issue.go.
func (r *Reconciler[Req, Resp, CB, T, PT]) reconcileIssue(ctx context.Context, issue *linearreconciler.Issue) error {
	ctx = clog.WithValues(ctx, "identifier", issue.Identifier, "title", issue.Title)

	// Check label gate.
	if r.requiredLabel != "" && !issue.HasLabel(r.requiredLabel) {
		clog.InfoContext(ctx, "Issue missing required label, skipping", "required_label", r.requiredLabel)
		return nil
	}

	// Single-Load gate stage: mirror the live Linear workflow-state into
	// State for downstream observability AND apply the bot-auto-cancel
	// reactivation reset, with a single Save if anything changed. Order
	// matters: this MUST run before the cancel-gate below so closed-issue
	// reconciles (which return nil from the cancel-gate without further
	// saves) still propagate the human's workflow-state flip to downstream
	// mirrors.
	//
	// Gate helpers are mutate-only — the Save fires once at the end of the
	// block with a single trigger (reactivation reset wins over linear-
	// state sync because the History entry should record the more
	// significant transition). Helpers are extracted so their predicates
	// are unit-testable without scaffolding a full reconcileIssue mock
	// chain.
	gateMgr := r.NewStateManager(issue)
	existing, loaded, gateLoadErr := gateMgr.Load(ctx)
	if gateLoadErr != nil {
		// Don't fail-closed on transient Load errors — fall through and
		// let the rest of reconcile run, where state mutations have their
		// own retry behaviour.
		clog.WarnContext(ctx, "Skipping state-based gates: load failed", "error", gateLoadErr)
	}

	gateTrigger := ""
	if loaded {
		if gateMirrorLinearWorkflowState[T, PT](existing, issue) {
			gateTrigger = TriggerLinearStateSync
		}
		if r.gateApplyReactivationReset(ctx, existing, issue) {
			gateTrigger = TriggerReactivated
		}
		if gateTrigger != "" {
			sctx := WithActor(ctx, r.identity)
			sctx = WithTrigger(sctx, gateTrigger)
			if _, err := gateMgr.Save(sctx, existing); err != nil {
				// Persist failure here degrades downstream-mirror freshness
				// for this reconcile but doesn't break correctness: the
				// next reconcile re-reads Linear's workflow state and
				// retries. Don't bail — fall through so the rest of
				// reconcile can still make progress on whatever the agent
				// path needs.
				clog.WarnContext(ctx, "Gate-stage save failed; downstream consumers may stay stale until next reconcile", "error", err, "trigger", gateTrigger)
			}
		}
	}

	// Cancel-gate: closed-issue reconciles bail without invoking the agent.
	// Runs AFTER the gate-stage save above so the human's "canceled"
	// workflow-state flip is captured in State (and mirrored to downstream
	// consumers) on the reconcile that observed it.
	if isLinearTerminal(issue.State.Type) {
		clog.InfoContext(ctx, "Issue is closed, skipping")
		return nil
	}

	// NoDiff/NoProgress terminal gate: bot decided no work was possible
	// (initial-run no-diff or K-iteration no-progress). These don't auto-
	// cancel Linear, so a human reactivating doesn't tell us anything new
	// — re-running would just reproduce the same no-op. Operators clear
	// State manually to retry, same escape hatch the framework already
	// documents.
	if loaded && existing.GetStatus() == StatusFailed &&
		(existing.GetFailureMode() == FailureModeNoDiff || existing.GetFailureMode() == FailureModeNoProgress) {
		clog.InfoContextf(ctx, "Skipping reconcile: previously transitioned to %s (terminal). Clear state to retry.", existing.GetFailureMode())
		return nil
	}

	// Resolve the GitHub repo target — first via upstream bot state
	// attachment, then via the optional fallback resolver if configured.
	target, err := r.resolveRepoTarget(ctx, issue)
	if err != nil {
		return fmt.Errorf("resolve repo target: %w", err)
	}

	owner, repo, err := splitOwnerRepo(target.Repo)
	if err != nil {
		return fmt.Errorf("parse repo target: %w", err)
	}

	ctx = clog.WithValues(ctx, "owner", owner, "repo", repo)

	// Construct a Resource using ResourceTypePath so that changemanager
	// derives a branch name from the Linear issue UUID (which is immutable,
	// unlike team-prefixed identifiers like ENG-123).
	res := &githubreconciler.Resource{
		Owner: owner,
		Repo:  repo,
		Type:  githubreconciler.ResourceTypePath,
		Path:  "linear-" + issue.ID,
		Ref:   "main",
	}

	// Get a GitHub client for this repo.
	gh, err := r.githubClients.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get GitHub client: %w", err)
	}

	// Create a change session which queries PR state via GraphQL.
	changeSession, err := r.changeManager.NewSession(ctx, gh, res)
	if err != nil {
		return fmt.Errorf("create change session: %w", err)
	}
	state := changeSession.State()

	// PR-terminal detection: changeSession.NewSession only queries OPEN PRs,
	// so when a PR has been closed or merged it appears as state.HasPR() ==
	// false. If we previously knew of a PR (PRURL set on State) but it's no
	// longer open, fetch it directly to determine merged-vs-closed and apply
	// the corresponding terminal transition. This runs here on the linear
	// workqueue's per-key serialization rather than via a parallel
	// github-workqueue handler, so Save calls for this Linear issue are
	// totally ordered and post-save observers see a consistent state machine.
	//
	// Steady-state cost is zero extra GitHub API calls per reconcile, not one:
	// after the terminal Save lands, subsequent reconciles short-circuit at
	// the gate above (Linear-terminal early return at the top of this
	// function, or the Failed/NoDiff|NoProgress skip in the gate stage)
	// before ever reaching here. The PR fetch only fires on the one
	// reconcile that observes the close.
	//
	// `loaded` is in the guard intentionally: on `gateLoadErr != nil` we
	// don't have a trustworthy `existing.GetPRURL()` (it would be a
	// zero-value PT), so we deliberately defer PR-terminal detection to
	// the next reconcile after a successful Load.
	if !state.HasPR() && loaded && existing.GetPRURL() != "" {
		prRes, perr := githubreconciler.ParseURL(existing.GetPRURL())
		if perr != nil {
			clog.WarnContext(ctx, "PR-terminal detection: cannot parse stored PRURL, falling through", "error", perr, "pr_url", existing.GetPRURL())
		} else {
			// prGH is intentionally a separate client lookup from the `gh`
			// client at line 148: gh is for the resolved-target repo
			// (owner/repo from resolveRepoTarget), while prGH is for whatever
			// owner/repo the stored PRURL parses to. Common case they're the
			// same; they'd legitimately diverge if the issue's resolved repo
			// target changed since the PR was created.
			prGH, perr := r.githubClients.Get(ctx, prRes.Owner, prRes.Repo)
			if perr != nil {
				return fmt.Errorf("PR-terminal detection: get GitHub client: %w", perr)
			}
			pr, resp, perr := prGH.PullRequests.Get(ctx, prRes.Owner, prRes.Repo, prRes.Number)
			if perr != nil {
				// 404 means the PR is no longer accessible (hard-deleted, repo
				// transferred, installation revoked). Without this special-case
				// we'd retry on the workqueue forever and the issue would stay
				// stuck. Treat the same as ParseURL failure above: warn and
				// fall through, letting the !state.HasPR() switch case below
				// create a fresh PR.
				if resp != nil && resp.StatusCode == http.StatusNotFound {
					clog.WarnContext(ctx, "PR-terminal detection: stored PR returned 404, falling through to fresh-PR path", "pr_url", existing.GetPRURL())
				} else {
					return fmt.Errorf("PR-terminal detection: fetch previously-known PR: %w", perr)
				}
			} else {
				// Reuse gateMgr (rather than constructing a fresh
				// r.NewStateManager) so StateManager's diff/History append
				// is computed against the load at the top of reconcileIssue,
				// not against a re-Loaded snapshot that could miss intervening
				// fields cleared by the gate stage (e.g. the reactivation
				// reset).
				if done, terr := r.maybeTransitionFromPRState(ctx, gateMgr, existing, pr); terr != nil {
					return terr
				} else if done {
					return nil
				}
				// PR is in a state we don't transition on (e.g., reopened); fall
				// through to normal flow. changeSession's switch handles the rest.
			}
		}
	}

	var (
		usePRBranch bool
		// trigger classifies why the agent is being invoked, recorded on the
		// History entry produced by the StatusActive transition below.
		trigger string
	)
	switch {
	case changeSession.ShouldSkip():
		clog.InfoContext(ctx, "PR should be skipped, not updating")
		return nil

	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")
		trigger = TriggerMergeConflict

	case state.HitMaxCommits():
		clog.InfoContext(ctx, "PR hit turn limit")
		// ApplyTurnLimit is idempotent: re-running it on workqueue retry
		// short-circuits when the turn-limit label is already present
		// (changemanager/session.go:208-211). It also returns the PR URL,
		// which we set on state alongside StatusFailed so the failed state
		// is always self-contained — no reliance on a prior StatusActive
		// Save having populated PRURL.
		prURL, err := changeSession.ApplyTurnLimit(ctx)
		if err != nil {
			return err
		}
		mgr := r.NewStateManager(issue)
		s, _, err := mgr.Load(ctx)
		if err != nil {
			return fmt.Errorf("load state: %w", err)
		}
		s.SetStatus(StatusFailed)
		s.SetFailureMode(FailureModeMaxTurns)
		if s.GetPRURL() == "" {
			s.SetPRURL(prURL)
		}
		ctx = WithActor(ctx, r.identity)
		ctx = WithTrigger(ctx, TriggerMaxTurns)
		_, err = mgr.Save(ctx, s)
		return err

	case state.HasFindings():
		// Findings-delta dedup: when the live finding set is identical to
		// what we last saw and persisted, the agent has already iterated
		// against this exact set of failures. Re-running it would burn
		// inference reproducing whatever it produced last time (commit or
		// no-diff). Webhook chatter on a stuck PR — success/neutral
		// check_run completions, label edits, reviewer activity — would
		// otherwise re-fire HasFindings on every event because the
		// level-based switch sees the lingering failures regardless of
		// which event triggered the reconcile.
		//
		// Skipping routes through saveNoDiffIterationState: it persists
		// the live findings/pending snapshot (no-op since they match the
		// existing record), increments the no-diff counter, and at
		// maxNoDiffIterations transitions to FailureModeNoProgress —
		// same backstop as a real no-diff agent run. The dedup is
		// strictly a cost optimisation; correctness still flows through
		// the existing terminal-state machinery.
		//
		// We only skip when the issue Description hash also matches the
		// hash embedded in PRData on the open PR. A human editing the
		// description while CI is failing is the documented signal for
		// redirecting the agent (Upsert.needsRefresh in
		// changemanager/session.go normally fires on this), and pre-PR
		// we always reached Upsert from this branch. Without the hash
		// gate, the dedup would silently swallow that redirect AND tick
		// the no-progress counter toward FailureModeNoProgress.
		liveFindings := snapshotFindings(changeSession.Findings())
		if loaded && len(existing.GetCurrentFindings()) > 0 && findingsEqual(liveFindings, existing.GetCurrentFindings()) {
			descChanged, derr := descriptionHashChanged(changeSession, issue)
			switch {
			case derr != nil:
				clog.WarnContext(ctx, "Findings dedup: description-hash check failed; falling through to agent run to be safe", "error", derr)
			case !descChanged:
				clog.InfoContext(ctx, "Findings unchanged and description unchanged; skipping agent run", "findings", len(liveFindings))
				return r.saveNoDiffIterationState(ctx, issue, liveFindings, changeSession.PendingChecks(), TriggerCIFailureIteration)
			default:
				clog.InfoContext(ctx, "Findings unchanged but issue description was edited; iterating to honour description redirect", "findings", len(liveFindings))
			}
		}
		clog.InfoContext(ctx, "PR has CI findings, iterating", "findings", len(liveFindings))
		usePRBranch = true
		trigger = TriggerCIFailureIteration

	case state.HasPendingChecks():
		// Pending checks are GitHub's domain (still computing, or
		// human-gated like CODEOWNERS). The agent has nothing to do
		// here — but we still snapshot the pending-check names onto
		// State so consumers can see what's blocking and bots'
		// BeforeSave hooks can derive workflow phase (e.g. "agent done,
		// only human-gated checks remain → ready for review").
		clog.InfoContext(ctx, "PR has pending checks, recording state and skipping iteration", "pending_checks", changeSession.PendingChecks())
		return r.savePendingChecksState(ctx, issue, changeSession.PendingChecks())

	case state.HasNoConflicts():
		// Don't return early -- fall through to Upsert which checks if the
		// description hash changed. If unchanged, Upsert is a no-op. If the
		// issue description was edited, the agent re-runs in iteration mode.
		clog.InfoContext(ctx, "PR is green, checking for description changes")
		usePRBranch = true
		trigger = TriggerDescriptionEditIteration

	case !state.HasPR():
		clog.InfoContext(ctx, "No existing PR, creating from scratch")
		trigger = TriggerInitialRun

	default:
		// Don't fall through to Upsert on an unrecognised state combination —
		// running the agent on stale context could clobber an existing PR.
		// Surface the gap loudly instead so it shows up in alerts.
		clog.ErrorContext(ctx, "Unexpected PR state combination, skipping reconciliation", "state", state)
		return nil
	}

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, issue, changeSession)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// agentRan tracks whether the inner callback (and therefore the agent)
	// actually executed. Upsert is a no-op when nothing changed (e.g. the
	// description hash is unchanged on the HasNoConflicts path); in that
	// case we want to avoid stamping the History entry with a trigger that
	// implies an iteration that never happened.
	var agentRan bool

	// agentNoDiffNote captures the agent's intended commit message when
	// the agent runs but produces no diff. Threaded out of the inner
	// closure for the Linear bot comment so the human triaging the issue
	// sees the agent's rationale rather than a bare failure pill.
	var agentNoDiffNote string

	// Create/update the PR with the changes. prData is passed by pointer and
	// the body template renders only after the closure below runs, so fields
	// set post-execution (ReasoningSummary) are visible to the template.
	prData := &PRData[Req]{
		Identity:         r.identity,
		LinearIssueID:    issue.ID,
		LinearIdentifier: issue.Identifier,
		DescriptionHash:  sha256.Sum256([]byte(issue.Description)),
		Request:          request,
	}
	prURL, err := changeSession.Upsert(ctx, prData, false, r.prLabels, func(ctx context.Context, branchName string) error {
		// Tee the agent's extended-thinking blocks off the trace so the PR
		// body template can render them via {{.ReasoningSummary}} (see
		// ReasoningSummarySnippet). No-op when the agent emits no reasoning.
		ctx, reasoning := agenttrace.CaptureReasoning[Resp](ctx)
		cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
		if err != nil {
			return fmt.Errorf("get clone manager: %w", err)
		}

		// Lease based on current state:
		// - CI failures on a mergeable PR: lease PR branch for iteration
		// - Otherwise (no PR, needs rebase, or fresh run): lease default branch
		var lease *clonemanager.Lease
		if usePRBranch {
			clog.InfoContext(ctx, "Acquiring clone lease for pull request branch", "branch", branchName)
			lease, err = cloneMgr.LeaseRef(ctx, res, branchName,
				clonemanager.WithCommitDepth(changeSession.CommitCount()+1))
		} else {
			clog.InfoContext(ctx, "Acquiring clone lease for default branch")
			lease, err = cloneMgr.Lease(ctx, res)
		}
		if err != nil {
			return fmt.Errorf("acquire lease: %w", err)
		}
		defer func() {
			if err := lease.Return(ctx); err != nil {
				// A failed lease return leaks a clone on disk; left
				// unchecked it eventually exhausts the pod's filesystem.
				// Log at Error so the alert pipeline picks it up.
				clog.ErrorContext(ctx, "Failed to return lease (clone may leak)", "error", err)
			}
		}()

		// Run the agent and push changes.
		return lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			cbs, err := r.buildCallbacks(ctx, changeSession, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			// Inject Linear-issue-id and trigger into context so bot-side
			// agent decorators (telemetry capture etc.) can correlate log
			// events without piggy-backing on the agent's request struct.
			agentCtx := WithLinearIssueID(ctx, issue.ID)
			agentCtx = WithTrigger(agentCtx, trigger)

			// Push an in-flight state snapshot to the downstream mirror BEFORE
			// the multi-minute agent run so post-save observers see the
			// agent has engaged (any wrapper-derived Phase computed by the
			// bot's trigger-driven BeforeSave updates immediately) while
			// the agent works, rather than the previous reconcile's stale
			// snapshot. NotifyProgress writes only the callback mirror — no
			// Linear attachment write — so this doesn't amplify the
			// duplicate-attachment race in linearreconciler/state.go.
			// Errors are logged but non-fatal: this is observability only,
			// never a gate on the agent. A transient Load failure here
			// commonly means a momentary GCS hiccup that the end-of-
			// reconcile Save will recover from; the rare case where it
			// signals deeper Linear connectivity (and the final Save fails
			// too) is already covered by the retriable-error path on the
			// outer workqueue dispatch.
			progressMgr := r.NewStateManager(issue)
			if progressState, _, perr := progressMgr.Load(agentCtx); perr != nil {
				clog.WarnContext(agentCtx, "In-flight progress notification: load failed (continuing with agent run)", "error", perr)
			} else if perr := progressMgr.NotifyProgress(agentCtx, progressState); perr != nil {
				clog.WarnContext(agentCtx, "In-flight progress notification failed (continuing with agent run)", "error", perr)
			}

			agentRan = true
			result, err := r.agent.Execute(agentCtx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			prData.ReasoningSummary = agenttrace.SummarizeReasoning(reasoning(), reasoningSummaryMaxChars)

			// No-diff detection: if the agent left the worktree clean, the
			// underlying worktree.Commit would return git.ErrEmptyCommit
			// and the workqueue would retry forever. Surfacing as
			// changemanager.ErrNoChanges lets the outer caller transition
			// State to StatusFailed + FailureModeNoDiff (terminal) instead.
			// The agent's intended commit message is captured in the
			// closure for the caller to record on the State.History entry.
			status, err := wt.Status()
			if err != nil {
				return "", fmt.Errorf("get worktree status: %w", err)
			}
			if status.IsClean() {
				agentNoDiffNote = result.GetCommitMessage()
				return "", changemanager.ErrNoChanges
			}
			return result.GetCommitMessage(), nil
		})
	})
	if err != nil {
		if errors.Is(err, changemanager.ErrNoChanges) {
			// "Agent produced no diff" means different things depending
			// on whether a PR already exists:
			//
			//   - Initial run (usePRBranch=false, no PR yet): agent
			//     reviewed and decided this issue needs no code changes.
			//     This is genuinely terminal — transition to
			//     StatusFailed + FailureModeNoDiff so downstream
			//     consumers see the failure mode and the workqueue
			//     stops retrying.
			//
			//   - Iteration on an existing PR (usePRBranch=true): agent
			//     re-ran (CI failure, description edit, etc.) and
			//     produced no further changes. The PR is still valid;
			//     we must NOT mark the issue failed and must NOT post a
			//     misleading "no changes needed" comment that would
			//     contradict the existing PR. Snapshot the live
			//     findings/pending the agent just observed so downstream
			//     consumers don't see a stale pre-iteration picture, then
			//     return nil so the workqueue stops retrying.
			if usePRBranch {
				clog.InfoContext(ctx, "Agent produced no diff on iteration; leaving existing PR unchanged")
				return r.saveNoDiffIterationState(ctx, issue, snapshotFindings(changeSession.Findings()), changeSession.PendingChecks(), trigger)
			}
			clog.InfoContext(ctx, "Agent produced no diff on initial run, transitioning to no_diff failure mode")
			return r.transitionToNoDiff(ctx, issue, agentNoDiffNote)
		}
		return fmt.Errorf("upsert PR: %w", err)
	}

	// If Upsert was a no-op (description hash unchanged on the
	// HasNoConflicts branch, etc.), the agent never ran — clear the trigger
	// so any History entry that fires (e.g. from a PRURL backfill) doesn't
	// claim an iteration that didn't happen.
	if !agentRan {
		trigger = ""
	}

	// StateManager.Save's (changed bool) gate is the gate for the bot
	// comment too: same status + same PRURL = no-op (no save, no comment,
	// no feedback loop on every reconcile). A fresh or recreated PR
	// triggers both. Save BEFORE posting the comment: if Save failed and
	// we'd already commented, the next reconcile would see no saved state
	// and comment again.
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.SetStatus(StatusActive)
	s.SetPRURL(prURL) // overwrite — fresh reconcile reflects the current PR

	// Snapshot the changemanager findings (CI failures + review threads)
	// and pending checks onto State so downstream consumers can see
	// what's failing AND what's still computing without querying GitHub
	// independently.
	//
	// Findings are sticky-forever by design: only updated when the live
	// snapshot is non-empty, so the field acts as a "last observed
	// failures" record useful as a retrospective signal on completed or
	// failed issues ("what was the last CI failure that blocked
	// progress?"). The framework never clears them on Status transitions
	// — reconciles that observe a green PR don't clobber the prior
	// snapshot, and a terminal Save preserves the trail. Bots that want
	// different semantics can override via SetCurrentFindings(nil) in
	// their BeforeSave hook.
	//
	// Pending checks are NOT sticky — always overwritten with the live
	// list so downstream consumers see the current pending set, not a
	// stale snapshot from a prior iteration.
	if newFindings := snapshotFindings(changeSession.Findings()); len(newFindings) > 0 {
		s.SetCurrentFindings(newFindings)
	}
	s.SetCurrentPendingChecks(changeSession.PendingChecks())
	// Agent ran and produced a diff (else we'd have hit the ErrNoChanges
	// branch above). Reset the no-progress counter so subsequent no-diff
	// iterations on this PR get a fresh K-attempt budget against a
	// different commit base.
	s.SetNoDiffIterationCount(0)

	ctx = WithActor(ctx, r.identity)
	ctx = WithTrigger(ctx, trigger)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if changed {
		body := fmt.Sprintf("PR created/updated: %s", prURL)
		if err := mgr.UpsertBotComment(ctx, body); err != nil {
			// State is saved; missing the comment is annoying but not
			// catastrophic. Best-effort.
			clog.WarnContext(ctx, "Failed to upsert bot comment", "error", err)
		}
	}

	clog.InfoContext(ctx, "PR created/updated", "pr_url", prURL)
	return nil
}

// transitionToNoDiff records a terminal StatusFailed + FailureModeNoDiff
// transition for an issue whose agent run produced no changes, and posts
// a Linear comment with the agent's rationale so the human triaging the
// issue knows what happened. Called from reconcileIssue when Upsert
// returns changemanager.ErrNoChanges.
//
// isLinearTerminal reports whether a Linear workflow-state value is one
// of the framework's recognised terminal types (completed/canceled).
// Pure helper so the predicate is unit-testable independent of reconcile
// flow.
func isLinearTerminal(stateType string) bool {
	return stateType == StateTypeCompleted || stateType == StateTypeCanceled
}

// gateMirrorLinearWorkflowState mutates `existing` so its LinearStateType
// and LinearStateName mirror the live Linear issue's workflow state.
// Returns true when fields were changed (caller persists), false on no-op.
//
// Mutate-only: the caller batches this with other gate-stage mutations
// into a single Save so a single reconcile produces at most one Linear
// write and one History entry per reconcile (instead of one per gate
// helper that fires).
//
// Refreshing this on EVERY reconcile — including reconciles that bail
// from the cancel-gate (closed-issue skip) — is the whole reason this
// helper exists: the bot's auto-cancel saveCallback only writes
// Failed/PRClosed when the bot itself observes a closed PR, so
// out-of-band cancellations driven from Linear UI / MCP / other sources
// would otherwise never reach State, leaving downstream mirrors stuck
// displaying a stale workflow-state forever.
func gateMirrorLinearWorkflowState[T any, PT StateConstraint[T]](existing PT, issue *linearreconciler.Issue) bool {
	if existing.GetLinearStateType() == issue.State.Type && existing.GetLinearStateName() == issue.State.Name {
		return false
	}
	existing.SetLinearStateType(issue.State.Type)
	existing.SetLinearStateName(issue.State.Name)
	return true
}

// gateApplyReactivationReset mutates `existing` to clear Failed/PRClosed
// terminal markers when a human has reactivated a Linear issue. Returns
// true if the reset was applied; false otherwise.
//
// Mutate-only: the caller batches this with gateMirrorLinearWorkflowState
// into a single Save. Predicate is:
//
//   - Linear must be non-terminal (cancel-gate fires below this stage,
//     so we explicitly check here rather than relying on it).
//   - State.Status==Failed && State.FailureMode==PRClosed.
//
// Predicate is intentionally narrow on the failure mode: only Failed/
// PRClosed warrants reset because the other terminal failure modes
// (NoDiff, NoProgress) don't auto-cancel Linear, so a human reactivating
// Linear gives no new signal — re-running would just reproduce the same
// no-op. Operators clear State manually to retry those, same escape hatch
// the framework already documents.
//
// Extracted from inline gate-stage code so the predicate is unit-testable
// without scaffolding a full reconcileIssue mock chain.
func (r *Reconciler[Req, Resp, CB, T, PT]) gateApplyReactivationReset(ctx context.Context, existing PT, issue *linearreconciler.Issue) bool {
	if isLinearTerminal(issue.State.Type) {
		return false
	}
	if existing.GetStatus() != StatusFailed || existing.GetFailureMode() != FailureModePRClosed {
		return false
	}
	clog.InfoContext(ctx, "Human reactivated Linear issue; clearing PR-closed terminal markers so the next reconcile starts fresh")
	existing.SetStatus(StatusActive)
	existing.SetFailureMode("")
	// Drop the stale PRURL so the switch in reconcileIssue hits the
	// !state.HasPR branch and creates a new PR. The closed PR's CI
	// findings/pending are no longer relevant — clear those too so
	// downstream consumers don't see stale failures while the new PR is
	// being constructed. The no-diff counter belongs to the closed PR's
	// iteration history; reset it for the fresh attempt.
	existing.SetPRURL("")
	existing.SetCurrentFindings(nil)
	existing.SetCurrentPendingChecks(nil)
	existing.SetNoDiffIterationCount(0)
	return true
}

// Returns nil so the workqueue does not retry — the agent has already
// decided the issue needs no work, and the same call would reproduce
// the same no-op. The TriggerDescriptionEditIteration path re-enters
// reconcile naturally when the human edits the issue description with
// new context.
func (r *Reconciler[Req, Resp, CB, T, PT]) transitionToNoDiff(ctx context.Context, issue *linearreconciler.Issue, note string) error {
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.SetStatus(StatusFailed)
	s.SetFailureMode(FailureModeNoDiff)
	ctx = WithActor(ctx, r.identity)
	ctx = WithTrigger(ctx, TriggerNoDiff)

	// Order matters: UpsertBotComment must run BEFORE Save. The framework's
	// linearreconciler.StateManager only persists the comment-tracking
	// commentID alongside the state attachment on the next Save call (see
	// linearreconciler/state.go). Saving first would persist an empty
	// commentID, then subsequent reconciles that re-enter this path would
	// post a fresh comment instead of updating the existing one. The
	// terminal-state gate at the top of reconcileIssue already prevents
	// most re-entry, but the order swap here is defence-in-depth.
	body := "I reviewed this issue but determined no code changes are needed."
	if note != "" {
		body = "I reviewed this issue but determined no code changes are needed:\n\n> " + note
	}
	if err := mgr.UpsertBotComment(ctx, body); err != nil {
		// Comment failure is logged but not fatal — the state transition
		// is the more important record. A failed comment also leaves
		// commentID empty in memory, which is fine; the state still
		// records the failure mode for downstream consumers.
		clog.WarnContext(ctx, "Failed to upsert no-diff bot comment", "error", err)
	}

	if _, err := mgr.Save(ctx, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// maybeTransitionFromPRState inspects a previously-known PR and, when it
// has reached a terminal GitHub state (merged or closed-without-merge),
// applies the corresponding State transition: merged → StatusComplete,
// closed → StatusFailed + FailureModePRClosed. Returns (true, nil) when
// a transition was applied (caller should bail from reconcile);
// (false, nil) when the PR is in a non-terminal or unexpected state
// (caller falls through to normal flow); (false, err) on a fatal Save
// error.
//
// Pure-helper signature (takes *github.PullRequest rather than fetching
// it itself) so callers can route the github API call however they like
// — the test suite passes constructed PR objects directly. Lives in the
// linear-workqueue path; replaces the github-workqueue's transitionPR
// (which raced cross-workqueue and produced inconsistent post-save
// snapshots).
func (r *Reconciler[Req, Resp, CB, T, PT]) maybeTransitionFromPRState(ctx context.Context, mgr *StateManager[T, PT], existing PT, pr *github.PullRequest) (bool, error) {
	var (
		targetStatus  Status
		targetMode    FailureMode
		targetTrigger string
	)
	switch {
	case pr.GetMerged():
		targetStatus = StatusComplete
		targetTrigger = TriggerPRMerge
		clog.InfoContext(ctx, "Previously-known PR was merged; transitioning to StatusComplete")
	case pr.GetState() == "closed":
		targetStatus = StatusFailed
		targetMode = FailureModePRClosed
		targetTrigger = TriggerPRClosed
		clog.InfoContext(ctx, "Previously-known PR was closed without merge; transitioning to StatusFailed/PRClosed")
	default:
		// Open or unexpected state — caller falls through to normal flow.
		return false, nil
	}
	existing.SetStatus(targetStatus)
	if targetMode != "" {
		existing.SetFailureMode(targetMode)
	}
	tctx := WithActor(ctx, r.identity)
	tctx = WithTrigger(tctx, targetTrigger)
	if _, err := mgr.Save(tctx, existing); err != nil {
		return false, fmt.Errorf("save terminal transition: %w", err)
	}
	return true, nil
}

// snapshotFindings projects the live changemanager findings into the
// FindingRef shape persisted on State. Returns nil for an empty input
// so the omitempty json tag drops the field cleanly when there's
// nothing to report.
func snapshotFindings(findings []callbacks.Finding) []FindingRef {
	if len(findings) == 0 {
		return nil
	}
	out := make([]FindingRef, 0, len(findings))
	for _, f := range findings {
		out = append(out, FindingRef{
			Kind:       string(f.Kind),
			Identifier: f.Identifier,
			Name:       f.Name,
			DetailsURL: f.DetailsURL,
		})
	}
	return out
}

// findingsEqual reports whether two FindingRef snapshots represent the same
// set of findings. The comparison is order-insensitive — GitHub doesn't
// guarantee a stable ordering across reconciles, so the inputs are sorted
// by (Kind, Identifier) before slices.Equal compares all fields.
//
// Strict by design: a check_run that re-runs gets a new GitHub job ID
// (Identifier), which makes it a different finding here. That intentionally
// fires the agent on retry rather than treating a flaky-then-failing-again
// check as "unchanged" — the alternative (matching by Name+Kind only)
// would mask matrix-job partial failures where the same Name fails on a
// different shard.
//
// Both inputs nil/empty returns true. Pure function so it's trivially
// testable independent of the rest of reconcileIssue.
func findingsEqual(a, b []FindingRef) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	aSorted := slices.Clone(a)
	bSorted := slices.Clone(b)
	cmp := func(x, y FindingRef) int {
		if c := strings.Compare(x.Kind, y.Kind); c != 0 {
			return c
		}
		return strings.Compare(x.Identifier, y.Identifier)
	}
	slices.SortFunc(aSorted, cmp)
	slices.SortFunc(bSorted, cmp)
	return slices.Equal(aSorted, bSorted)
}

// descriptionHashChanged reports whether the issue's current description
// differs from the DescriptionHash embedded in PRData on the open PR.
//
// Returns false (and a nil error) when there is no open PR to compare
// against, or when the PRData marker can't be extracted — the
// findings-dedup path callers treat both as "no signal of a description
// edit," and Upsert.needsRefresh remains the canonical fallback for
// detecting redirects when we do reach it on subsequent reconciles.
//
// Returns a non-nil error only on cryptographic failure (which can't
// happen for sha256.Sum256 in practice). Callers fall through to a real
// agent run on any error to be safe rather than silently swallowing the
// human's redirect.
func descriptionHashChanged[Req any](changeSession *changemanager.Session[PRData[Req]], issue *linearreconciler.Issue) (bool, error) {
	existingData, err := changeSession.Extract()
	if err != nil {
		return false, fmt.Errorf("extract PRData: %w", err)
	}
	if existingData == nil {
		return false, nil
	}
	currentHash := sha256.Sum256([]byte(issue.Description))
	return currentHash != existingData.DescriptionHash, nil
}

// maxNoDiffIterations is the cap on consecutive no-diff iterations against
// the same PR before the framework gives up and transitions the issue to
// StatusFailed + FailureModeNoProgress. Hardcoded rather than configurable
// because the value isn't tuning-sensitive: the loop the cap exists to break
// is dominated by webhook bursts on stuck PRs, not by legitimate iteration
// patterns. Bumpable in a follow-up if real-world data warrants.
const maxNoDiffIterations = 3

// saveNoDiffIterationState persists the live findings/pending snapshot the
// agent just observed when it iterated on an existing PR but produced no
// further changes, increments the no-diff iteration counter, and transitions
// to StatusFailed + FailureModeNoProgress when the counter reaches the cap.
// Status and PRURL are otherwise untouched (the PR is still valid and active).
//
// The trigger argument is the trigger that drove this iteration
// (TriggerCIFailureIteration / TriggerDescriptionEditIteration /
// TriggerMergeConflict) and is threaded through context so bot wrappers'
// BeforeSave hooks see "agent did engage this reconcile" and refresh
// LastIterationAt + Phase accordingly. When the cap-induced transition fires,
// the trigger is overridden to TriggerNoProgress so the History entry reflects
// the framework decision rather than the originating webhook.
//
// Findings/pending changes alone do NOT trigger a Linear attachment write
// (the dirty check covers Status/FailureMode/PRURL only), but the post-save
// callback fires on every Save call so downstream mirrors capture the fresh
// observation. Without this save, downstream consumers would see the stale
// pre-iteration snapshot — typically empty findings from the initial-run
// save before CI had even started — even though the framework just spent
// minutes iterating on real CI failures. The cap-induced Status/FailureMode
// transition does flip the dirty check, so the cap save is always persisted
// to Linear.
func (r *Reconciler[Req, Resp, CB, T, PT]) saveNoDiffIterationState(ctx context.Context, issue *linearreconciler.Issue, findings []FindingRef, pendingChecks []string, trigger string) error {
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	// Sticky-findings rule: only update when the live snapshot is non-empty,
	// so a transient empty observation (e.g. a check that briefly cleared)
	// doesn't clobber the prior failure record.
	if len(findings) > 0 {
		s.SetCurrentFindings(findings)
	}
	s.SetCurrentPendingChecks(pendingChecks)

	next := s.GetNoDiffIterationCount() + 1
	s.SetNoDiffIterationCount(next)
	if next >= maxNoDiffIterations {
		s.SetStatus(StatusFailed)
		s.SetFailureMode(FailureModeNoProgress)
		trigger = TriggerNoProgress
		clog.InfoContextf(ctx, "Agent produced no diff on %d consecutive iterations; transitioning to no_progress failure mode", next)
	}

	ctx = WithActor(ctx, r.identity)
	ctx = WithTrigger(ctx, trigger)
	if _, err := mgr.Save(ctx, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// savePendingChecksState updates State's CurrentPendingChecks snapshot
// for a reconcile that didn't run the agent (HasPendingChecks branch).
// The agent didn't iterate, so PRURL/findings/status aren't refreshed —
// just the pending list, so consumers can see what's blocking.
//
// The Linear write is gated by mgr.Save's existing dirty check (no-op
// when no framework field changed), but the post-save callback fires
// regardless so downstream mirrors stay current. Bots' BeforeSave hooks
// see the updated pending list and can derive their own workflow phase.
//
// Trigger is intentionally empty: the agent didn't run, so there's no
// History entry to attribute. Bot wrappers that key off TriggerFromContext
// can use the empty trigger as their "agent didn't iterate this time"
// signal.
func (r *Reconciler[Req, Resp, CB, T, PT]) savePendingChecksState(ctx context.Context, issue *linearreconciler.Issue, pendingChecks []string) error {
	mgr := r.NewStateManager(issue)
	s, _, err := mgr.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.SetCurrentPendingChecks(pendingChecks)
	// Findings are intentionally NOT cleared here — they're sticky
	// across reconciles by design (CurrentFindings = "last observed
	// failures", not "currently failing right now"). A reconcile that
	// reaches HasPendingChecks just means CI is recomputing; the prior
	// failure record remains useful as retrospective signal until a new
	// failure replaces it or the issue terminally completes.
	ctx = WithActor(ctx, r.identity)
	if _, err := mgr.Save(ctx, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
