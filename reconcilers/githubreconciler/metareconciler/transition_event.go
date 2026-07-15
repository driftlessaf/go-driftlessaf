/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/statemachine"
	"github.com/google/go-github/v88/github"
)

// StateTransitionEventType is the CloudEvent type emitted once per
// state-machine transition. Alias of [statemachine.StateTransitionEventType];
// see the statemachine package for the contract documentation.
const StateTransitionEventType = statemachine.StateTransitionEventType

// StateTransitionEvent is the wire payload for StateTransitionEventType.
// Alias of [statemachine.StateTransitionEvent].
type StateTransitionEvent = statemachine.StateTransitionEvent

// stateTransitionProvider is the Provider value stamped on every event this
// package emits. The event type is a framework-wide contract shared with
// other metareconcilers (the Linear one describes the same PR lifecycle);
// Provider is the discriminator that lets them all land in one BigQuery
// table without a v2 of the type.
const stateTransitionProvider = "github"

// GitHub-flavor Trigger values, complementing the shared statemachine set
// (the Linear metareconciler likewise defines provider-specific triggers
// alongside its flow).
const (
	// TriggerReadyForReview is the trigger when a green PR is handed to human
	// review (the ready-for-review label is newly applied). The status stays
	// active — from here on, elapsed time without a transition measures how
	// long the PR has waited on humans.
	TriggerReadyForReview = "ready_for_review"
	// TriggerIssueClosed is the trigger when the reconciler observes its
	// issue closed. The issue's state reason decides the status: complete
	// when closed as completed, failed otherwise (not planned / duplicate).
	TriggerIssueClosed = "issue_closed"
	// TriggerRequiredLabelRemoved is the trigger when the required label is
	// removed from the issue and the reconciler closes its outstanding PR,
	// abandoning the work.
	TriggerRequiredLabelRemoved = "required_label_removed"
)

// emitTransition sends one state-transition CloudEvent, best-effort. Unlike
// the Linear metareconciler — whose state machine is persisted and diffed at
// save time — the GitHub flow keeps no prior state, so callers invoke this
// at the edges they can observe from GitHub itself (a state label newly
// applied, a PR created or pushed to, the issue closed) and FromStatus is
// always empty. A no-op when emission is not wired (nil emitter).
func (r *Reconciler[Req, Resp, CB]) emitTransition(ctx context.Context, issue *github.Issue, prURL string, to statemachine.Status, mode statemachine.FailureMode, trigger string) {
	if r.transitionEmitter == nil {
		return
	}
	r.transitionEmitter.Emit(ctx, StateTransitionEvent{
		Bot:      r.transitionEmitter.Source(),
		Provider: stateTransitionProvider,
		// The GitHub flavor keys work by issue URL (see the statemachine
		// contract: IssueID is provider-native).
		IssueID:      issue.GetHTMLURL(),
		IssueURL:     issue.GetHTMLURL(),
		PRURL:        prURL,
		ToStatus:     to,
		FailureMode:  mode,
		Actor:        r.identity,
		Trigger:      trigger,
		TransitionAt: time.Now().UTC(),
	})
}

// closedIssueTransition maps a closed-issue observation to its terminal
// status. A PR still open at observation time means the issue closed
// underneath in-flight work, which the reconcile is now abandoning
// (failed/pr_closed) regardless of why the issue closed. With no open PR the
// issue's state reason decides: "completed" is the merge auto-close (or an
// operator marking the work done) — complete; anything else (not planned,
// duplicate, or an older issue predating state reasons) means the work was
// discarded — failed, with no failure mode since none of the agent-derived
// modes describe a human closing the issue.
func closedIssueTransition(hadPR bool, stateReason string) (statemachine.Status, statemachine.FailureMode) {
	switch {
	case hadPR:
		return statemachine.StatusFailed, statemachine.FailureModePRClosed
	case stateReason == "completed":
		return statemachine.StatusComplete, ""
	default:
		return statemachine.StatusFailed, ""
	}
}

// upsertTrigger classifies which flow led the reconcile to fall through to
// Upsert (an agent run that pushed commits): a first run with no PR yet, a
// CI-findings iteration on the PR branch, or a regeneration after a merge
// conflict. An unexpected state combination (the switch's default branch)
// carries no trigger.
func upsertTrigger(wasNoPR, usePRBranch, needsRebase bool) string {
	switch {
	case wasNoPR:
		return statemachine.TriggerInitialRun
	case usePRBranch:
		return statemachine.TriggerCIFailureIteration
	case needsRebase:
		return statemachine.TriggerMergeConflict
	default:
		return ""
	}
}
