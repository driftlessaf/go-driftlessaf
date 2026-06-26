/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
)

// Reconciler is a generic reconciler for metaagents.
type Reconciler[Req promptbuilder.Bindable, Resp Result, CB any] struct {
	identity      string
	changeManager *changemanager.CM[PRData[Req]]
	cloneMeta     *clonemanager.Meta
	prLabels      []string

	// requiredLabel is checked before processing an issue. If set and the issue
	// doesn't have this label, reconciliation is skipped. This allows filtering
	// to only process issues managed by a specific identity.
	requiredLabel string

	// prLabelsFromResult derives extra labels to stamp on the generated PR from
	// the agent result. Opt-in: nil means no extra labels are added.
	prLabelsFromResult func(Resp) []string

	// copyIssueLabels, when true, copies every label from the source issue onto
	// the generated PR. Opt-in: false means no issue labels are carried over.
	// Unlike prLabels (a fixed set always stamped), this lets labels be applied
	// to the PR situationally, by labeling the issue.
	copyIssueLabels bool

	// giveUp, when set, surfaces an agent's deliberate no-op explanation on the
	// PR as a single marker comment. Nil is a safe no-op receiver. See
	// WithGiveUpComment.
	giveUp *changemanager.GiveUpComment

	// Agent and its adapters
	agent          metaagent.Agent[Req, Resp, CB]
	buildRequest   func(context.Context, *github.Issue, *changemanager.Session[PRData[Req]]) (Req, error)
	buildCallbacks func(context.Context, *changemanager.Session[PRData[Req]], *clonemanager.Lease) (CB, error)
}

// Option configures a Reconciler.
type Option[Req promptbuilder.Bindable, Resp Result, CB any] func(*Reconciler[Req, Resp, CB])

// WithRequiredLabel configures the reconciler to only process issues that have
// the specified label. Issues without this label are skipped during reconciliation.
func WithRequiredLabel[Req promptbuilder.Bindable, Resp Result, CB any](label string) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.requiredLabel = label
	}
}

// WithPRLabelsFromResult configures the reconciler to stamp extra labels on the
// generated PR, derived from the agent result. The labels are added after Upsert
// succeeds, when the PR number is known. A nil or empty return adds nothing.
func WithPRLabelsFromResult[Req promptbuilder.Bindable, Resp Result, CB any](fn func(Resp) []string) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.prLabelsFromResult = fn
	}
}

// WithCopyIssueLabels configures the reconciler to copy every label from the
// source issue onto the generated PR. This lets labels be applied to PRs
// situationally — by labeling the issue — rather than always (which is what
// passing them in prLabels does). The issue labels are merged with prLabels on
// every Upsert, so adding a label to an issue propagates it to an already-open
// PR on the next reconcile. Off by default.
func WithCopyIssueLabels[Req promptbuilder.Bindable, Resp Result, CB any]() Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.copyIssueLabels = true
	}
}

// WithGiveUpComment surfaces an agent's deliberate no-op on the PR. When the
// agent runs but makes no file changes and its result implements
// changemanager.Explainer with a non-empty explanation, the reconciler upserts
// a single comment (identified by marker) whose body is render(explanation).
// Repeated identical give-ups rewrite nothing, and the comment is cleared when
// the PR recovers. Off by default.
func WithGiveUpComment[Req promptbuilder.Bindable, Resp Result, CB any](marker string, render func(explanation string) string) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.giveUp = &changemanager.GiveUpComment{Marker: marker, Render: render}
	}
}

// New creates a new generic metaagent reconciler.
func New[Req promptbuilder.Bindable, Resp Result, CB any](
	identity string,
	changeManager *changemanager.CM[PRData[Req]],
	cloneMeta *clonemanager.Meta,
	prLabels []string,
	agent metaagent.Agent[Req, Resp, CB],
	buildRequest func(context.Context, *github.Issue, *changemanager.Session[PRData[Req]]) (Req, error),
	buildCallbacks func(context.Context, *changemanager.Session[PRData[Req]], *clonemanager.Lease) (CB, error),
	opts ...Option[Req, Resp, CB],
) *Reconciler[Req, Resp, CB] {
	r := &Reconciler[Req, Resp, CB]{
		identity:       identity,
		changeManager:  changeManager,
		cloneMeta:      cloneMeta,
		prLabels:       prLabels,
		agent:          agent,
		buildRequest:   buildRequest,
		buildCallbacks: buildCallbacks,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Reconcile processes an issue or pull request URL.
// For issues: runs the agent to create/update a PR.
// For PRs: finds linked issues with the required label and queues them for processing.
func (r *Reconciler[Req, Resp, CB]) Reconcile(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	switch res.Type {
	case githubreconciler.ResourceTypeIssue:
		return r.reconcileIssue(ctx, res, gh)
	case githubreconciler.ResourceTypePullRequest:
		return r.reconcilePullRequest(ctx, res, gh)
	default:
		clog.WarnContext(ctx, "Unexpected resource type", "type", res.Type)
		return nil
	}
}
