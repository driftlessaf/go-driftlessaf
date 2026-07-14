/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import (
	"context"
	"time"

	"github.com/chainguard-dev/clog"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
)

// StateTransitionEventType is the CloudEvent type emitted once per
// state-machine transition (a Status or FailureMode change). It gives every
// metareconciler bot a shared, dashboard-readable state channel: wiring the
// recorder-schemas output of agents/agenttrace/iac into a cloudevent
// recorder archives these events to BigQuery, where per-bot live state and
// stuck-in-state queries become one GROUP BY instead of a per-bot
// integration. Provider flavors share this one type — the payload's
// Provider field is the discriminator.
const StateTransitionEventType = "dev.chainguard.driftlessaf.state.transition.v1"

// sendTimeout bounds the broker send so a slow broker can never stall a
// reconcile; the event is best-effort observability.
const sendTimeout = 10 * time.Second

// StateTransitionEvent is the wire payload for StateTransitionEventType.
// Flat by design: it lands in a BigQuery recorder table that dashboards
// query directly, so nesting would only complicate the schema.
type StateTransitionEvent struct {
	// Bot is the reconciler identity that performed the transition.
	Bot string `json:"bot"`
	// Provider identifies which metareconciler flavor emitted the event
	// (e.g. "linear", "github"). It discriminates provider-specific
	// fields — IssueID semantics and the linear_state_* columns — so all
	// providers share the event type and BigQuery table.
	Provider string `json:"provider"`
	// IssueID identifies the unit of work whose state machine
	// transitioned, in provider-native form (Linear: the issue UUID;
	// GitHub: the issue URL).
	IssueID string `json:"issue_id"`
	// IssueURL is the human-facing issue URL, when known.
	IssueURL string `json:"issue_url,omitempty"`
	// PRURL is the pull request the state machine is tracking, when one exists.
	PRURL string `json:"pr_url,omitempty"`
	// FromStatus and ToStatus are the endpoints of the transition.
	FromStatus Status `json:"from_status"`
	ToStatus   Status `json:"to_status"`
	// FailureMode qualifies ToStatus when it is StatusFailed.
	FailureMode FailureMode `json:"failure_mode,omitempty"`
	// Actor and Trigger mirror the StateTransition history entry.
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

// Emitter sends StateTransitionEvents to a CloudEvents broker.
//
// Emission is opt-in and explicitly wired: the bot's main declares the
// broker configuration (typically EVENT_INGRESS_URI via its envconfig)
// and passes a client in. This package reads no environment.
type Emitter struct {
	source string // bot identity; CloudEvent source
	client cloudevents.Client
}

// NewEmitter builds an emitter for the given bot identity. A nil client
// (e.g. agenttrace.NewBrokerClient with an empty URI) disables emission
// and returns nil; Emit is safe on the nil result.
func NewEmitter(identity string, client cloudevents.Client) *Emitter {
	if client == nil {
		return nil
	}
	return &Emitter{source: identity, client: client}
}

// Source returns the bot identity the emitter stamps as the CloudEvent
// source. Callers use it to keep the payload's Bot field and the envelope
// source in lockstep.
func (e *Emitter) Source() string {
	return e.source
}

// Emit sends one transition event, best-effort. Never returns an error:
// state persistence has already succeeded and observability must not undo
// that. Safe on a nil receiver.
func (e *Emitter) Emit(ctx context.Context, ev StateTransitionEvent) {
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
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	if result := e.client.Send(sendCtx, ce); !cloudevents.IsACK(result) {
		clog.WarnContext(ctx, "State-transition event send failed",
			"bot", e.source,
			"issue_id", ev.IssueID,
			"error", result,
		)
	}
}
