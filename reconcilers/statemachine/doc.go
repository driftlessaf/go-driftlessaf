/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package statemachine holds the reconciler state-machine vocabulary shared
// across metareconciler flavors: the Status/FailureMode value sets, the
// StateTransition record, the actor/trigger context plumbing, and the
// state-transition CloudEvent contract (StateTransitionEvent + Emitter).
//
// The vocabulary describes one bot's progress on one unit of work tracked
// against a pull request: active while the agent iterates, complete when the
// PR merges, failed with a FailureMode otherwise. It was extracted from
// linearreconciler/metareconciler (which re-exports it via aliases, so
// existing consumers compile unchanged) so that other metareconcilers can
// emit the same dev.chainguard.driftlessaf.state.transition.v1 events into
// the same recorder table, discriminated by the event's Provider field.
//
// Persistence is provider-specific and lives with each metareconciler: the
// Linear flavor persists State to a Linear attachment; other flavors bring
// their own store. This package deliberately holds no persistence.
package statemachine
