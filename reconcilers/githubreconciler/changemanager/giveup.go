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
// any package can compose it against their own change session. ApplyGaveUp and
// ClearGaveUp keep a give-up label in lockstep with the give-up comment.
type markerCommenter interface {
	UpsertMarkerComment(ctx context.Context, marker, body string) error
	DeleteMarkerComment(ctx context.Context, marker string) error
	ApplyGaveUp(ctx context.Context) (string, error)
	ClearGaveUp(ctx context.Context) (string, error)
}

// issueMarkerCommenter is the subset of Session that the issue-side surface
// (SurfaceResultOnIssue, ClearIssue) needs, for reconciles with no PR yet to
// carry the explanation.
type issueMarkerCommenter interface {
	UpsertIssueMarkerCommentForRevision(ctx context.Context, marker, revision, body string) (bool, error)
	DeleteIssueMarkerComment(ctx context.Context, marker string) error
}

// GiveUpComment surfaces an agent's deliberate no-op explanation on a PR as a
// single marker-identified comment, then clears it once the condition no longer
// holds (the agent recovered). It is framework-agnostic: any reconciler holding
// a change Session can compose it, independent of metapathreconciler or
// metareconciler. The marker comment dedups (an identical repeat give-up
// rewrites nothing) and degrades to a no-op when comment-write permission is
// missing — see Session.UpsertMarkerComment.
//
// When the agent gives up before any PR exists there is no PR to comment on;
// SurfaceResultOnIssue and ClearIssue carry the same explanation on the source
// issue instead, under the same Marker.
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
	g.Surface(ctx, pr, explanationOf(result))
}

// explanationOf returns the no-change explanation carried by result, or "" when
// result is not an Explainer or is a typed-nil pointer.
func explanationOf(result any) string {
	// A typed-nil pointer still satisfies Explainer, so guard against it before
	// calling the accessor: an implementation with a pointer receiver would
	// panic on a nil receiver.
	if rv := reflect.ValueOf(result); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return ""
	}
	ex, ok := result.(Explainer)
	if !ok {
		return ""
	}
	return ex.GetNoChangeExplanation()
}

// SurfaceResultOnIssue is SurfaceResult for a reconcile with no PR to comment
// on: it posts the explanation carried by result on the source issue instead,
// so the filer learns why nothing happened without reading logs. revision
// identifies the issue content the agent saw (see
// Session.UpsertIssueMarkerCommentForRevision); a comment already posted for
// the same revision is left as is, since the agent's wording varies run to run
// while its reason does not. It reports whether a comment was posted or
// rewritten, so a caller can treat that as the give-up edge. No label is
// applied — the give-up label belongs to the PR. Failures are logged and
// swallowed.
func (g *GiveUpComment) SurfaceResultOnIssue(ctx context.Context, issue issueMarkerCommenter, revision string, result any) bool {
	if g == nil || g.Render == nil {
		return false
	}
	explanation := explanationOf(result)
	if explanation == "" {
		return false
	}
	changed, err := issue.UpsertIssueMarkerCommentForRevision(ctx, g.Marker, revision, g.Render(explanation))
	if err != nil {
		clog.WarnContext(ctx, "Failed to post give-up comment on issue", "error", err)
		return false
	}
	return changed
}

// ClearIssue removes the give-up comment from the source issue once it no
// longer applies (the agent went on to open a PR, or a human took the issue
// back). Failures are logged and swallowed.
func (g *GiveUpComment) ClearIssue(ctx context.Context, issue issueMarkerCommenter) {
	if g == nil {
		return
	}
	if err := issue.DeleteIssueMarkerComment(ctx, g.Marker); err != nil {
		clog.WarnContext(ctx, "Failed to clear give-up comment on issue", "error", err)
	}
}

// Surface posts or updates the give-up comment with the given explanation and
// applies the give-up label. An empty explanation is a no-op. Failures are
// logged and swallowed. The label is only applied after the comment is
// posted, so a labeled PR always has a matching give-up comment.
func (g *GiveUpComment) Surface(ctx context.Context, pr markerCommenter, explanation string) {
	if g == nil || g.Render == nil || explanation == "" {
		return
	}
	if err := pr.UpsertMarkerComment(ctx, g.Marker, g.Render(explanation)); err != nil {
		clog.WarnContext(ctx, "Failed to post give-up comment", "error", err)
		return
	}
	if _, err := pr.ApplyGaveUp(ctx); err != nil {
		clog.WarnContext(ctx, "Failed to apply give-up label", "error", err)
	}
}

// Clear removes the give-up label and comment when the agent recovers.
// Label first, comment second: a failed label-clear leaves both in place,
// preserving Surface's invariant that a labeled PR has a matching comment.
// Failures are logged and swallowed.
func (g *GiveUpComment) Clear(ctx context.Context, pr markerCommenter) {
	if g == nil {
		return
	}
	if _, err := pr.ClearGaveUp(ctx); err != nil {
		clog.WarnContext(ctx, "Failed to clear give-up label", "error", err)
		return
	}
	if err := pr.DeleteMarkerComment(ctx, g.Marker); err != nil {
		clog.WarnContext(ctx, "Failed to clear give-up comment", "error", err)
	}
}
