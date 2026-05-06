/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
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
)

// Trigger values the framework records on State.History entries.
// Downstream consumers of the persisted state can match against these
// constants rather than hardcoding strings. Bots are free to define their
// own additional trigger constants for transitions they drive themselves.
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
	IssueID     string            `json:"issue_id,omitempty"`
	IssueURL    string            `json:"issue_url,omitempty"`
	PRURL       string            `json:"pr_url,omitempty"`
	Status      Status            `json:"status,omitempty"`
	FailureMode FailureMode       `json:"failure_mode,omitempty"`
	History     []StateTransition `json:"history,omitempty"`
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
// and before the dirty check. The default implementation is a no-op
// returning false.
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
	// SetIssueID and SetIssueURL are called by StateManager.Save before
	// every persist, with values captured from the *Issue at
	// NewStateManager time. Bots never call these themselves — when a
	// bot's wrapper value-embeds State, the methods are promoted and
	// satisfied automatically.
	SetIssueID(string)
	SetIssueURL(string)
	// BeforeSave is called by StateManager.Save immediately before each
	// persist; bots populate wrapper-specific fields here. The default
	// no-op on State is satisfied automatically via embedding; bots that
	// need custom behavior shadow it on their wrapper. See State.BeforeSave
	// for return-value semantics.
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
