/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"slices"
	"sort"
	"time"
)

// Status is a Linear-issue-driven reconciler's progress state on an issue. It
// is a distinct string type so callers cannot assign arbitrary strings (typos
// like "actve" become compile-time errors).
type Status string

// Framework-defined Status values, set by the framework's reconcile flow.
// Bots that need additional statuses for bot-driven phases declare them as
// `const MyStatus metareconciler.Status = "..."` in the bot package.
const (
	StatusActive   Status = "active"
	StatusComplete Status = "complete"
	StatusFailed   Status = "failed"
)

// FailureMode classifies why a State landed on StatusFailed. Only the modes
// we have detection paths for today are defined; new modes are added when
// their detection logic lands.
type FailureMode string

// FailureMode values persisted alongside StatusFailed on the state attachment.
const (
	// FailureModeMaxTurns means the agent exhausted its commit budget without
	// converging on a green PR. The PR also gets a "turn-limit" label.
	FailureModeMaxTurns FailureMode = "max_turns"
	// FailureModePRClosed means a human closed the PR without merging it,
	// abandoning the work.
	FailureModePRClosed FailureMode = "pr_closed"
	// FailureModeNoDiff means the agent ran successfully but produced no
	// changes (clean working tree). Treated as terminal because retrying
	// without external input (an issue description edit, a comment, etc.)
	// would just reproduce the same no-op. Operators can re-trigger by
	// editing the issue description, which the framework picks up via
	// TriggerDescriptionEditIteration.
	FailureModeNoDiff FailureMode = "no_diff"
	// FailureModeNoProgress means the agent has produced no diff on
	// maxNoDiffIterations consecutive iterations of the same PR. The
	// framework gives up so it doesn't burn agent inference re-trying a
	// stuck PR on every check_run webhook. Operators clear State to retry,
	// same escape hatch as FailureModeNoDiff.
	FailureModeNoProgress FailureMode = "no_progress"
)

// Trigger values the framework records on State.History entries.
// Downstream consumers of the persisted state can match against these
// constants rather than hardcoding strings. Bots are free to define their
// own additional trigger constants for transitions they drive themselves.
//
// MAINTAINER NOTE — bot-side classification: triggers fall into two
// classes that bots' BeforeSave hooks may need to distinguish:
//
//   - Agent-engagement triggers — the agent ran (or was about to run)
//     on this reconcile. Bots typically bump iteration markers and
//     observability fields here. Today: TriggerInitialRun,
//     TriggerCIFailureIteration, TriggerDescriptionEditIteration,
//     TriggerMergeConflict, TriggerMaxTurns, TriggerNoDiff,
//     TriggerNoProgress.
//
//   - Framework-observation triggers — the framework wrote State as a
//     side effect of observing external state (PR webhook, Linear-
//     state mirror, reactivation reset). The agent did NOT engage on
//     this reconcile, so bots should typically skip iteration-marker
//     bumps and treat the trigger as "no agent activity" for derived
//     observability fields. Today: TriggerPRMerge, TriggerPRClosed,
//     TriggerReactivated, TriggerLinearStateSync.
//
// When adding a new Trigger constant below, classify it in the right
// bucket and update consumers that switch on the classification —
// e.g. bots/linear-materializer's frameworkObservationTriggers set
// in internal/state/state.go. The framework can't enforce this at
// compile time (the set lives in downstream packages), so the
// reminder lives here as a tripwire for code reviewers.
const (
	// TriggerInitialRun is the trigger for the first reconcile of an issue:
	// no PR exists yet, the agent is being invoked from scratch.
	TriggerInitialRun = "initial_run"
	// TriggerCIFailureIteration is the trigger for re-running the agent
	// because the existing PR has CI failures to address.
	TriggerCIFailureIteration = "ci_failure_iteration"
	// TriggerDescriptionEditIteration is the trigger for re-running the
	// agent because the Linear issue description changed since the last
	// successful PR.
	TriggerDescriptionEditIteration = "description_edit_iteration"
	// TriggerMergeConflict is the trigger when the existing PR has merge
	// conflicts with its base branch and the agent is being invoked to
	// regenerate from a fresh checkout of the default branch.
	TriggerMergeConflict = "merge_conflict"
	// TriggerMaxTurns is the trigger recorded when the agent has hit its
	// commit budget on a PR; the issue transitions to StatusFailed with
	// FailureModeMaxTurns.
	TriggerMaxTurns = "max_turns"
	// TriggerPRMerge is the trigger when a PR-event handler observes a
	// merged PR; the issue transitions to StatusComplete.
	TriggerPRMerge = "pr_merge"
	// TriggerPRClosed is the trigger when a PR-event handler observes a
	// closed-without-merge PR; the issue transitions to StatusFailed with
	// FailureModePRClosed.
	TriggerPRClosed = "pr_closed"
	// TriggerNoDiff is the trigger recorded when the agent completes with
	// a clean working tree; the issue transitions to StatusFailed with
	// FailureModeNoDiff. The agent's intended commit message is captured
	// on the StateTransition's Note field for operator visibility.
	TriggerNoDiff = "no_diff"
	// TriggerNoProgress is the trigger recorded when the no-diff iteration
	// counter reaches maxNoDiffIterations and the issue transitions to
	// StatusFailed + FailureModeNoProgress.
	TriggerNoProgress = "no_progress"
	// TriggerReactivated is the trigger recorded when a human moves a
	// previously-canceled Linear issue back to a non-terminal workflow
	// state and the framework clears the StatusFailed + FailureModePRClosed
	// markers so the bot can re-engage. Only fires for the PRClosed flavour
	// of canceled — NoDiff and NoProgress remain operator-cleared.
	TriggerReactivated = "reactivated"
	// TriggerLinearStateSync is set on the gate-stage Save's context when
	// reconcile observes a Linear workflow-state change. Visible to post-
	// save callback consumers via TriggerFromContext; NOT appended to
	// State.History — mirror-only changes don't transition Status/Mode,
	// and StateManager.Save only appends a History entry on
	// statusOrModeChanged (manager.go), so a from==to entry would be
	// misleading observability.
	//
	// When a single reconcile observes both a workflow-state change AND
	// the bot-auto-cancel reactivation reset condition, the gate-stage
	// Save uses TriggerReactivated instead — reactivation does
	// transition Status/Mode and so DOES appear in History as the
	// canonical record of the more significant transition.
	//
	// Distinct from TriggerReactivated in semantics: sync fires for ANY
	// workflow-state change (started → in_review, canceled →
	// in_progress, etc.) whereas reactivated only fires when clearing
	// bot auto-cancel markers.
	TriggerLinearStateSync = "linear_state_sync"
)

// State is the framework's persistent state-machine record for a single
// Linear issue. Bots that need bot-specific fields define their own State
// type that value-embeds this one; the embedded fields are promoted to the
// top level of the JSON serialization, so a bot's wrapper persists as a
// single flat blob in one Linear attachment.
//
// History is appended automatically by StateManager.Save when Status or
// FailureMode changes — callers do not write to it directly.
//
// IssueID and IssueURL are framework-managed: StateManager.Save injects
// them on every save (captured from the Linear *Issue at NewStateManager
// time) so downstream consumers reading the attachment have everything
// needed to render a clickable Linear link without an extra API call.
// Bots never set these themselves; on Load they're populated automatically
// by JSON deserialization.
type State struct {
	IssueID              string            `json:"issue_id,omitempty"`
	IssueURL             string            `json:"issue_url,omitempty"`
	PRURL                string            `json:"pr_url,omitempty"`
	Status               Status            `json:"status,omitempty"`
	FailureMode          FailureMode       `json:"failure_mode,omitempty"`
	History              []StateTransition `json:"history,omitempty"`
	CurrentFindings      []FindingRef      `json:"current_findings,omitempty"`
	CurrentPendingChecks []string          `json:"current_pending_checks,omitempty"`
	// NoDiffIterationCount is the number of consecutive iterations on the
	// current PR where the agent ran but produced no diff. Reset to 0 on
	// any iteration that produces a commit. When it reaches
	// maxNoDiffIterations, the reconciler transitions to StatusFailed +
	// FailureModeNoProgress. Cleared implicitly when an operator resets
	// State to retry (the whole attachment is replaced).
	//
	// jsonschema:"minimum=0" is a hint to schema generators that the
	// framework only ever sets this counter to non-negative values; a
	// negative value at rest indicates corruption (manual mis-edit,
	// downstream-store byte-mangling) and downstream consumers can
	// schema-validate to surface it. The framework itself doesn't
	// import jsonschema; the tag is purely metadata.
	NoDiffIterationCount int `json:"no_diff_iteration_count,omitempty" jsonschema:"minimum=0"`
	// LinearStateType mirrors the Linear workflow-state type observed at
	// the most recent reconcile (e.g. "started", "canceled", "completed",
	// "triage", "backlog", "unstarted"). Refreshed at the top of every
	// reconcile so downstream mirrors see the human's workflow-state
	// changes even when the framework's reconcile flow returns nil from
	// a gate (closed-issue skip, etc.). The framework manages this
	// field — bots never set it themselves.
	LinearStateType string `json:"linear_state_type,omitempty"`
	// LinearStateName mirrors the human-readable Linear workflow-state
	// name observed at the most recent reconcile (e.g. "In Progress",
	// "Canceled", "Done"). Same lifecycle as LinearStateType; carried
	// alongside the type so downstream consumers can render the actual
	// team-configured label without re-querying Linear.
	LinearStateName string `json:"linear_state_name,omitempty"`
}

// pendingChecksCap bounds the persisted CurrentPendingChecks list to
// keep the attachment small. PRs typically have a handful of pending
// checks at most; the cap is a defence against a misconfigured repo
// with hundreds of check_runs flooding the snapshot. SetCurrentPendingChecks
// sorts the input alphabetically before truncating, so the persisted
// subset is deterministic across reconciles even if the caller-supplied
// order shifts (changemanager appends in GitHub-return order, which can
// vary across pagination passes).
const pendingChecksCap = 50

// FindingRef is a stable, persistence-friendly projection of a
// changemanager finding (CI failure, code-review thread). The full
// in-process Finding struct carries pre-fetched details that can be
// large and volatile; FindingRef captures only what consumers need to
// render the failure and link back to its source.
//
// Kind is left as a plain string (matching the underlying
// callbacks.FindingKind values "ciCheck" and "review") rather than a
// typed enum so consumers don't need to import the toolcall package
// just to read State, and so future finding kinds slot in without a
// framework change.
type FindingRef struct {
	Kind       string `json:"kind"`
	Identifier string `json:"identifier"`
	// Name is a short human-readable label suitable for rendering by
	// downstream consumers (e.g. CI check name "Lint" or review locator
	// "src/foo.go:42"). Empty when the upstream Finding had no Name set,
	// in which case consumers typically fall back to Identifier.
	Name       string `json:"name,omitempty"`
	DetailsURL string `json:"details_url,omitempty"`
}

// SetIssueID sets the Linear issue UUID. Part of the StateMachine interface.
// Called by StateManager.Save with the value captured at NewStateManager
// time; bots never invoke this directly.
func (s *State) SetIssueID(id string) { s.IssueID = id }

// SetIssueURL sets the canonical Linear issue URL. Part of the StateMachine
// interface. Called by StateManager.Save with the value captured at
// NewStateManager time; bots never invoke this directly.
func (s *State) SetIssueURL(u string) { s.IssueURL = u }

// GetStatus returns the current Status. Part of the StateMachine interface.
func (s *State) GetStatus() Status { return s.Status }

// SetStatus sets the Status. Part of the StateMachine interface.
func (s *State) SetStatus(st Status) { s.Status = st }

// GetPRURL returns the current PRURL. Part of the StateMachine interface.
func (s *State) GetPRURL() string { return s.PRURL }

// SetPRURL sets the PRURL. Part of the StateMachine interface.
func (s *State) SetPRURL(u string) { s.PRURL = u }

// GetFailureMode returns the current FailureMode. Part of the StateMachine interface.
func (s *State) GetFailureMode() FailureMode { return s.FailureMode }

// GetCurrentFindings returns the most recent findings snapshot. Part of
// the StateMachine interface.
func (s *State) GetCurrentFindings() []FindingRef { return s.CurrentFindings }

// SetCurrentFindings replaces the findings snapshot wholesale. Part of
// the StateMachine interface. Callers pass nil/empty to clear (e.g. when
// the PR is green again). The input is cloned so later mutations by the
// caller don't drift the persisted snapshot or the StateManager's diff
// baseline.
func (s *State) SetCurrentFindings(f []FindingRef) {
	s.CurrentFindings = slices.Clone(f)
}

// GetCurrentPendingChecks returns the most recent pending-checks snapshot.
// Part of the StateMachine interface.
func (s *State) GetCurrentPendingChecks() []string { return s.CurrentPendingChecks }

// SetCurrentPendingChecks replaces the pending-checks snapshot wholesale.
// Part of the StateMachine interface. Callers pass nil/empty to clear.
//
// Sorts a clone of the input alphabetically and drops entries past
// pendingChecksCap. Sorting makes the persisted subset deterministic
// across reconciles (changemanager assembles the list in GitHub-return
// order, which is not stable). Cloning prevents mutating the caller's
// underlying array and keeps the StateManager's slice-aliased diff
// snapshot stable.
func (s *State) SetCurrentPendingChecks(p []string) {
	if len(p) == 0 {
		s.CurrentPendingChecks = nil
		return
	}
	clone := slices.Clone(p)
	sort.Strings(clone)
	if len(clone) > pendingChecksCap {
		clone = clone[:pendingChecksCap]
	}
	s.CurrentPendingChecks = clone
}

// GetNoDiffIterationCount returns the current consecutive-no-diff counter.
// Part of the StateMachine interface.
func (s *State) GetNoDiffIterationCount() int { return s.NoDiffIterationCount }

// SetNoDiffIterationCount sets the consecutive-no-diff counter. Part of the
// StateMachine interface. The framework increments on each no-diff iteration
// and resets to 0 on any iteration that produces a commit.
func (s *State) SetNoDiffIterationCount(n int) { s.NoDiffIterationCount = n }

// GetLinearStateType returns the most-recently-observed Linear workflow-
// state type. Part of the StateMachine interface.
func (s *State) GetLinearStateType() string { return s.LinearStateType }

// SetLinearStateType records the Linear workflow-state type observed at
// reconcile. Part of the StateMachine interface; framework-managed.
func (s *State) SetLinearStateType(t string) { s.LinearStateType = t }

// GetLinearStateName returns the most-recently-observed Linear workflow-
// state human-readable name. Part of the StateMachine interface.
func (s *State) GetLinearStateName() string { return s.LinearStateName }

// SetLinearStateName records the Linear workflow-state name observed at
// reconcile. Part of the StateMachine interface; framework-managed.
func (s *State) SetLinearStateName(n string) { s.LinearStateName = n }

// SetFailureMode sets the FailureMode. Part of the StateMachine interface.
func (s *State) SetFailureMode(fm FailureMode) { s.FailureMode = fm }

// historyCap bounds State.History to keep the persisted attachment well
// below Linear's per-attachment limit (~10MB; see maxAttachmentSize in
// linearreconciler/client.go). At ~250 bytes per StateTransition, 200
// entries is ~50KB. When the cap is exceeded, the oldest entries are
// dropped (FIFO). Downstream mirrors are the place to keep unbounded
// transition history for analytics.
const historyCap = 200

// AppendHistory appends a StateTransition to History, FIFO-evicting the
// oldest entries when the slice exceeds historyCap. Part of the StateMachine
// interface. Callers should generally not invoke this directly —
// StateManager.Save appends transitions automatically when Status or
// FailureMode changes.
func (s *State) AppendHistory(t StateTransition) {
	s.History = append(s.History, t)
	if len(s.History) > historyCap {
		s.History = s.History[len(s.History)-historyCap:]
	}
}

// GetHistory returns the History slice. Part of the StateMachine interface.
// Used by StateManager's post-save hook to surface the just-appended
// StateTransition without re-querying the attachment.
func (s *State) GetHistory() []StateTransition { return s.History }

// BeforeSave is called by StateManager.Save immediately before each persist,
// after the framework has set its own fields (Status, PRURL, IssueID/URL)
// and before the dirty check. It is also invoked by StateManager.NotifyProgress
// (without a Linear write) so in-flight callback snapshots carry the same
// derived fields downstream consumers see on persisted snapshots. As a
// result a single reconcile path may invoke BeforeSave multiple times on
// distinct State instances — implementations MUST be idempotent and pure
// derivation (no counter increments, no resource allocation, no side
// effects beyond setting wrapper-specific fields). The default
// implementation is a no-op returning false.
//
// Bots that add wrapper-specific fields to their embedded-State struct can
// shadow this method on their wrapper to populate those fields from
// reconcile context (e.g. trigger via TriggerFromContext, actor via
// ActorFromContext, current time). BeforeSave should mutate only
// wrapper-specific fields, never the framework state-machine fields owned
// by SetStatus/SetPRURL/SetFailureMode — those are driven by the
// reconcile flow.
//
// The return value controls whether the mutation forces a Linear save:
//
//   - return false: cosmetic / observability-only mutation. The Linear
//     attachment is not written unless something else (Status, FailureMode,
//     or PRURL) also changed; the post-save callback fires either way so
//     downstream mirrors capture the new field values.
//   - return true: meaningful enough to persist on Linear even when no
//     framework-tracked field changed. Use sparingly — every true return is
//     an extra Linear write.
//
// Method shadowing in Go: when a bot's wrapper struct value-embeds State
// and defines its own BeforeSave method on the wrapper, that method is
// chosen by the StateMachine interface dispatch instead of this default.
// Bots that don't shadow get the no-op for free.
func (s *State) BeforeSave(_ context.Context) bool { return false }

// StateMachine is the contract a bot's State type must satisfy to be managed
// by StateManager. It is satisfied automatically when a bot's wrapper struct
// value-embeds State (or pointer-embeds *State).
type StateMachine interface {
	GetStatus() Status
	SetStatus(Status)
	GetPRURL() string
	SetPRURL(string)
	GetFailureMode() FailureMode
	SetFailureMode(FailureMode)
	AppendHistory(StateTransition)
	GetHistory() []StateTransition
	// GetCurrentFindings / SetCurrentFindings expose the most recent
	// changemanager findings snapshot (CI failures + review threads).
	// StateManager.Save persists the slice as-is; the reconcile loop
	// refreshes it on every reconcile that produces findings, including
	// passing nil to clear when the PR is green.
	GetCurrentFindings() []FindingRef
	SetCurrentFindings([]FindingRef)
	GetCurrentPendingChecks() []string
	SetCurrentPendingChecks([]string)
	// GetNoDiffIterationCount / SetNoDiffIterationCount expose the
	// consecutive-no-diff counter. The framework increments on each no-diff
	// iteration and resets to 0 on any iteration that produces a commit;
	// when the counter reaches maxNoDiffIterations the reconciler
	// transitions to StatusFailed + FailureModeNoProgress.
	GetNoDiffIterationCount() int
	SetNoDiffIterationCount(int)
	// GetLinearStateType / SetLinearStateType expose the Linear
	// workflow-state type observed at reconcile (e.g. "started",
	// "canceled"). Framework-managed: refreshed at the top of every
	// reconcile so downstream mirrors see human workflow-state changes
	// — including out-of-band cancellations driven from Linear UI / MCP
	// that the bot's saveCallback never observes.
	GetLinearStateType() string
	SetLinearStateType(string)
	// GetLinearStateName / SetLinearStateName expose the human-
	// readable Linear workflow-state name (e.g. "In Progress",
	// "Canceled"). Framework-managed alongside LinearStateType.
	GetLinearStateName() string
	SetLinearStateName(string)
	// SetIssueID and SetIssueURL are called by StateManager.Save before
	// every persist, with values captured from the *Issue at
	// NewStateManager time. Bots never call these themselves — when a
	// bot's wrapper value-embeds State, the methods are promoted and
	// satisfied automatically.
	SetIssueID(string)
	SetIssueURL(string)
	// BeforeSave is called by StateManager.Save immediately before each
	// persist AND by StateManager.NotifyProgress before each callback-only
	// snapshot push, so a single reconcile path may invoke it multiple
	// times on distinct State instances. Bots populate wrapper-specific
	// fields here; implementations MUST be idempotent and free of side
	// effects beyond setting those fields. The default no-op on State is
	// satisfied automatically via embedding; bots that need custom behavior
	// shadow it on their wrapper. See State.BeforeSave for return-value
	// semantics and the idempotency contract.
	BeforeSave(context.Context) bool
}

// StateConstraint is the generic constraint linking a value type T to its
// pointer type *T satisfying StateMachine. Generic functions in this package
// use the (T, PT) pair so they can construct fresh instances via `var t T;
// pt := PT(&t)` while still calling pointer-receiver methods through PT.
type StateConstraint[T any] interface {
	*T
	StateMachine
}

// StateTransition records a single Status (and optional FailureMode) change.
// Append-only via StateManager.Save's automatic diff detection.
type StateTransition struct {
	From    Status      `json:"from,omitempty"`
	To      Status      `json:"to"`
	At      time.Time   `json:"at"`
	Actor   string      `json:"actor,omitempty"`   // bot identity, or "manual" for human edits
	Trigger string      `json:"trigger,omitempty"` // e.g. "pr_merge", "pr_closed", "max_turns"
	Note    string      `json:"note,omitempty"`
	Mode    FailureMode `json:"mode,omitempty"` // populated alongside To=StatusFailed
}
