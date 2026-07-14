/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import "chainguard.dev/driftlessaf/reconcilers/statemachine"

// StateTransitionEventType is the CloudEvent type emitted once per
// state-machine transition. Alias of [statemachine.StateTransitionEventType];
// see the statemachine package for the contract documentation.
const StateTransitionEventType = statemachine.StateTransitionEventType

// StateTransitionEvent is the wire payload for StateTransitionEventType.
// Alias of [statemachine.StateTransitionEvent].
type StateTransitionEvent = statemachine.StateTransitionEvent

// stateTransitionProvider is the Provider value stamped on every event this
// package emits. The event type is a framework-wide contract shared with
// other metareconcilers (the GitHub one describes the same PR lifecycle);
// Provider is the discriminator that lets them all land in one BigQuery
// table without a v2 of the type.
const stateTransitionProvider = "linear"
