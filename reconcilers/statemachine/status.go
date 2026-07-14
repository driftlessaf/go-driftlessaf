/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import "time"

// Status is a reconciler's progress state on one unit of work (a Linear
// issue, a GitHub issue, ...). It is a distinct string type so callers
// cannot assign arbitrary strings (typos like "actve" become compile-time
// errors).
type Status string

// Framework-defined Status values, set by the framework's reconcile flow.
// Bots that need additional statuses for bot-driven phases declare them as
// `const MyStatus statemachine.Status = "..."` in the bot package.
const (
	StatusActive   Status = "active"
	StatusComplete Status = "complete"
	StatusFailed   Status = "failed"
)

// FailureMode classifies why a state landed on StatusFailed. Only the modes
// we have detection paths for today are defined; new modes are added when
// their detection logic lands.
type FailureMode string

// FailureMode values persisted alongside StatusFailed.
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

// Trigger values the framework records on StateTransition entries.
// Downstream consumers of persisted state can match against these constants
// rather than hardcoding strings. Bots are free to define their own
// additional trigger constants for transitions they drive themselves, and
// metareconciler packages define provider-specific triggers alongside
// their flow (e.g. the Linear metareconciler's TriggerLinearStateSync).
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
//   - Framework-observation triggers — the framework wrote state as a
//     side effect of observing external state (PR webhook, workflow-
//     state mirror, reactivation reset). The agent did NOT engage on
//     this reconcile, so bots should typically skip iteration-marker
//     bumps and treat the trigger as "no agent activity" for derived
//     observability fields. Today: TriggerPRMerge, TriggerPRClosed,
//     plus the Linear metareconciler's TriggerReactivated and
//     TriggerLinearStateSync.
//
// When adding a new Trigger constant below, classify it in the right
// bucket and update downstream consumers that switch on the
// classification (bots typically keep a framework-observation trigger
// set beside their BeforeSave hook). The framework can't enforce this
// at compile time (those sets live in downstream packages), so the
// reminder lives here as a tripwire for code reviewers.
const (
	// TriggerInitialRun is the trigger for the first reconcile of an issue:
	// no PR exists yet, the agent is being invoked from scratch.
	TriggerInitialRun = "initial_run"
	// TriggerCIFailureIteration is the trigger for re-running the agent
	// because the existing PR has CI failures to address.
	TriggerCIFailureIteration = "ci_failure_iteration"
	// TriggerDescriptionEditIteration is the trigger for re-running the
	// agent because the issue description changed since the last
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
)

// StateTransition records a single Status (and optional FailureMode) change.
// Append-only via each metareconciler's save-time diff detection.
type StateTransition struct {
	From    Status      `json:"from,omitempty"`
	To      Status      `json:"to"`
	At      time.Time   `json:"at"`
	Actor   string      `json:"actor,omitempty"`   // bot identity, or "manual" for human edits
	Trigger string      `json:"trigger,omitempty"` // e.g. "pr_merge", "pr_closed", "max_turns"
	Note    string      `json:"note,omitempty"`
	Mode    FailureMode `json:"mode,omitempty"` // populated alongside To=StatusFailed
}
