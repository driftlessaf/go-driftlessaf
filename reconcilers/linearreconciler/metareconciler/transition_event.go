/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"time"

	"github.com/chainguard-dev/clog"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
)

// StateTransitionEventType is the CloudEvent type emitted once per
// state-machine transition (a Status or FailureMode change recorded on
// State.History). It gives every metareconciler bot a shared,
// dashboard-readable state channel: the factory recorder writes these
// events to BigQuery (see agents/agenttrace/iac), where per-bot live state
// and stuck-in-state queries become one GROUP BY instead of a per-bot
// Firestore integration.
const StateTransitionEventType = "dev.chainguard.driftlessaf.state.transition.v1"

// stateTransitionProvider is the Provider value stamped on every event this
// package emits. The event type above is a framework-wide contract shared
// with other metareconcilers (the GitHub one describes the same PR
// lifecycle); Provider is the discriminator that lets them all land in one
// BigQuery table without a v2 of the type.
const stateTransitionProvider = "linear"

// stateTransitionSendTimeout bounds the broker send so a slow broker can
// never stall a reconcile; the event is best-effort observability.
const stateTransitionSendTimeout = 10 * time.Second

// StateTransitionEvent is the wire payload for StateTransitionEventType.
// Flat by design: it lands in a BigQuery recorder table that dashboards
// query directly, so nesting would only complicate the schema.
type StateTransitionEvent struct {
	// Bot is the reconciler identity that performed the transition.
	Bot string `json:"bot"`
	// Provider identifies which metareconciler flavor emitted the event
	// ("linear" for this package). It discriminates provider-specific
	// fields — IssueID semantics and the linear_state_* columns here —
	// so other providers (e.g. the GitHub metareconciler) can share the
	// event type and BigQuery table.
	Provider string `json:"provider"`
	// IssueID is the Linear issue UUID whose state machine transitioned.
	IssueID string `json:"issue_id"`
	// IssueURL is the Linear issue URL, when known.
	IssueURL string `json:"issue_url,omitempty"`
	// PRURL is the pull request the state machine is tracking, when one exists.
	PRURL string `json:"pr_url,omitempty"`
	// FromStatus and ToStatus are the endpoints of the transition.
	FromStatus Status `json:"from_status"`
	ToStatus   Status `json:"to_status"`
	// FailureMode qualifies ToStatus when it is StatusFailed.
	FailureMode FailureMode `json:"failure_mode,omitempty"`
	// Actor and Trigger mirror the StateTransition History entry.
	Actor   string `json:"actor,omitempty"`
	Trigger string `json:"trigger,omitempty"`
	// LinearStateType and LinearStateName mirror the human-facing Linear
	// workflow state observed at the transition. Provider-specific
	// (Provider == "linear"); other providers leave them empty.
	LinearStateType string `json:"linear_state_type,omitempty"`
	LinearStateName string `json:"linear_state_name,omitempty"`
	// TransitionAt is when the transition was recorded. BigQuery partition field.
	TransitionAt time.Time `json:"transition_at"`
}

// transitionEmitter sends StateTransitionEvents to a CloudEvents broker.
//
// Emission is opt-in and explicitly wired: the bot's main declares the
// broker configuration (typically EVENT_INGRESS_URI via its envconfig)
// and passes a client through WithStateTransitionEmission. The library
// itself reads no environment.
type transitionEmitter struct {
	source string // bot identity; CloudEvent source
	client cloudevents.Client
}

// newTransitionEmitter builds an emitter for the given bot identity.
// A nil client (e.g. agenttrace.NewBrokerClient with an empty URI)
// disables emission and returns nil.
func newTransitionEmitter(identity string, client cloudevents.Client) *transitionEmitter {
	if client == nil {
		return nil
	}
	return &transitionEmitter{source: identity, client: client}
}

// emit sends one transition event, best-effort. Never returns an error:
// state persistence has already succeeded and observability must not undo
// that. Safe on a nil receiver.
func (e *transitionEmitter) emit(ctx context.Context, ev StateTransitionEvent) {
	if e == nil {
		return
	}

	ce := cloudevents.NewEvent()
	ce.SetID(uuid.NewString())
	ce.SetType(StateTransitionEventType)
	ce.SetSource(e.source)
	ce.SetTime(ev.TransitionAt)
	if err := ce.SetData(cloudevents.ApplicationJSON, ev); err != nil {
		clog.WarnContext(ctx, "State-transition event dropped: encode failed", "error", err)
		return
	}

	// Detach from the reconcile's cancellation: a transition observed at
	// the tail of a reconcile should still reach the broker.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateTransitionSendTimeout)
	defer cancel()
	if result := e.client.Send(sendCtx, ce); !cloudevents.IsACK(result) {
		clog.WarnContext(ctx, "State-transition event send failed",
			"bot", e.source,
			"issue_id", ev.IssueID,
			"error", result,
		)
	}
}
