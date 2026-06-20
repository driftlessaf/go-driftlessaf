/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"reflect"

	"github.com/chainguard-dev/clog"
)

// Explainer is implemented by agent result types that can explain a deliberate
// no-op (the agent ran but had no in-scope change to make). A GiveUpComment uses
// it to surface that reasoning on the PR instead of letting it vanish into logs.
type Explainer interface {
	GetNoChangeExplanation() string
}

// markerCommenter is the subset of Session that GiveUpComment needs. It is an
// interface so the helper is testable without a full session and so callers in
// any package can compose it against their own change session.
type markerCommenter interface {
	UpsertMarkerComment(ctx context.Context, marker, body string) error
	DeleteMarkerComment(ctx context.Context, marker string) error
}

// GiveUpComment surfaces an agent's deliberate no-op explanation on a PR as a
// single marker-identified comment, then clears it once the condition no longer
// holds (the agent recovered). It is framework-agnostic: any reconciler holding
// a change Session can compose it, independent of metapathreconciler or
// metareconciler. The marker comment dedups (an identical repeat give-up
// rewrites nothing) and degrades to a no-op when comment-write permission is
// missing — see Session.UpsertMarkerComment.
//
// A nil *GiveUpComment is a valid no-op receiver, so reconcilers can hold an
// always-present field and call its methods unconditionally.
type GiveUpComment struct {
	// Marker is the hidden HTML marker identifying the comment, so repeated
	// give-ups update one comment in place rather than posting duplicates.
	Marker string
	// Render formats the agent's explanation into the comment body. It must be
	// non-nil; Surface is a no-op when it is nil (rather than panicking inside a
	// reconcile loop).
	Render func(explanation string) string
}

// SurfaceResult posts the give-up explanation carried by result, when result
// implements Explainer with a non-empty explanation. A typed-nil result (e.g.
// an agent that never ran) is ignored. Failures are logged and swallowed:
// surfacing an explanation must not turn a clean no-op into a reconcile error.
func (g *GiveUpComment) SurfaceResult(ctx context.Context, pr markerCommenter, result any) {
	if g == nil {
		return
	}
	// A typed-nil pointer still satisfies Explainer, so guard against it before
	// calling the accessor: an implementation with a pointer receiver would
	// panic on a nil receiver.
	if rv := reflect.ValueOf(result); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return
	}
	ex, ok := result.(Explainer)
	if !ok {
		return
	}
	g.Surface(ctx, pr, ex.GetNoChangeExplanation())
}

// Surface posts or updates the give-up comment with the given explanation. An
// empty explanation is a no-op. Failures are logged and swallowed.
func (g *GiveUpComment) Surface(ctx context.Context, pr markerCommenter, explanation string) {
	if g == nil || g.Render == nil || explanation == "" {
		return
	}
	if err := pr.UpsertMarkerComment(ctx, g.Marker, g.Render(explanation)); err != nil {
		clog.WarnContext(ctx, "Failed to post give-up comment", "error", err)
	}
}

// Clear removes the give-up comment left by a prior iteration once the agent
// recovers (pushes a fix, or the PR otherwise resolves). No-op when no matching
// comment exists. Failures are logged and swallowed.
func (g *GiveUpComment) Clear(ctx context.Context, pr markerCommenter) {
	if g == nil {
		return
	}
	if err := pr.DeleteMarkerComment(ctx, g.Marker); err != nil {
		clog.WarnContext(ctx, "Failed to clear give-up comment", "error", err)
	}
}
