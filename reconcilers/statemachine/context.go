/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package statemachine

import "context"

// Generic context plumbing for state-machine bookkeeping and bot-side
// telemetry correlation. The keys carry opaque strings — the framework does
// not endorse any particular vocabulary for actor identities or trigger
// names. Bots and the framework agree on values out-of-band.

type actorKey struct{}
type triggerKey struct{}

// WithActor returns a context carrying the actor identity (e.g. the bot's
// own identity, or "manual" for human edits). Metareconciler save paths
// read this when appending a StateTransition to history.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFromContext returns the actor identity stored on ctx, and true when
// one is present.
func ActorFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(actorKey{}).(string)
	return v, ok
}

// WithTrigger returns a context carrying the trigger string for the next
// state transition (see Trigger* constants in this package, e.g.
// TriggerPRMerge, TriggerCIFailureIteration). Metareconciler save paths
// read this when appending to history; bots set it before calling Save.
func WithTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, triggerKey{}, trigger)
}

// TriggerFromContext returns the trigger string stored on ctx, and true
// when one is present.
func TriggerFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(triggerKey{}).(string)
	return v, ok
}
