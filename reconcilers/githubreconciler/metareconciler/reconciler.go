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
	"chainguard.dev/driftlessaf/reconcilers/statemachine"
	"github.com/chainguard-dev/clog"
	cloudevents "github.com/cloudevents/sdk-go/v2"
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

	// startComment, when set, posts a single marker comment on the source issue
	// the first time the bot reconciles it with no PR yet, announcing that work
	// has started. Nil means disabled. See WithStartComment.
	startComment *startComment

	// transitionEmitter publishes StateTransitionEvents to the CloudEvents
	// broker at the state edges reconcileIssue observes. Nil means emission
	// is disabled (the default). See WithStateTransitionEmission.
	transitionEmitter *statemachine.Emitter

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

// issueMarkerCommenter is the subset of Session that startComment needs. It is
// an interface so surface is testable without a full session.
type issueMarkerCommenter interface {
	UpsertIssueMarkerComment(ctx context.Context, marker, body string) error
}

// startComment holds the configuration for the opt-in issue start comment: a
// hidden HTML marker that dedups the comment across reconciles, and a render
// func producing its body.
type startComment struct {
	marker string
	render func() string
}

// surface posts or updates the start comment on the issue. Failures are logged
// and swallowed: announcing work has started must never fail the reconcile.
func (c *startComment) surface(ctx context.Context, issue issueMarkerCommenter) {
	if c == nil {
		return
	}
	if err := issue.UpsertIssueMarkerComment(ctx, c.marker, c.render()); err != nil {
		clog.WarnContext(ctx, "Failed to post start comment", "error", err)
	}
}

// WithStateTransitionEmission supplies the CloudEvents client for
// state-transition emission (StateTransitionEventType). Emission is off
// unless this option provides a non-nil client; the library reads no
// environment. Bot mains typically declare the broker URL in their
// envconfig (conventionally EVENT_INGRESS_URI) and pass
// agenttrace.NewBrokerClient(ctx, uri) here — that constructor returns nil
// on an empty URI, so the option can be supplied unconditionally and
// emission follows the environment's wiring.
//
// Producers on federated/WIF credentials cannot use the default
// NewBrokerClient path (idtoken cannot mint from an external-account
// credential); pass agenttrace.NewBrokerClientImpersonating instead.
func WithStateTransitionEmission[Req promptbuilder.Bindable, Resp Result, CB any](client cloudevents.Client) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		// New sets identity before applying options, so the emitter can
		// stamp it as the CloudEvent source here.
		r.transitionEmitter = statemachine.NewEmitter(r.identity, client)
	}
}

// WithStartComment posts a single comment on the source issue when the bot
// first reconciles it and no PR exists yet, announcing that work has started.
// The comment body is render(), prefixed with the hidden HTML marker so
// repeated no-PR reconciles dedup to one comment rather than posting again.
// Once a PR exists the comment is never posted, so new commits on an open PR do
// not retrigger it. Off by default.
func WithStartComment[Req promptbuilder.Bindable, Resp Result, CB any](marker string, render func() string) Option[Req, Resp, CB] {
	return func(r *Reconciler[Req, Resp, CB]) {
		r.startComment = &startComment{marker: marker, render: render}
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
