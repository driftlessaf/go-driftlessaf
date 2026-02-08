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
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
)

// Reconciler is a generic reconciler for metaagents.
type Reconciler[Req promptbuilder.Bindable, Resp Result, CB any] struct {
	identity      string
	changeManager *changemanager.CM[PRData]
	cloneMeta     *clonemanager.Meta
	prLabels      []string

	// requiredLabel is checked before processing an issue. If set and the issue
	// doesn't have this label, reconciliation is skipped. This allows filtering
	// to only process issues managed by a specific identity.
	requiredLabel string

	// Agent and its adapters
	agent          metaagent.Agent[Req, Resp, CB]
	buildRequest   func(*github.Issue, *changemanager.Session[PRData]) Req
	buildCallbacks func(*gogit.Worktree, *changemanager.Session[PRData]) CB
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

// New creates a new generic metaagent reconciler.
func New[Req promptbuilder.Bindable, Resp Result, CB any](
	identity string,
	changeManager *changemanager.CM[PRData],
	cloneMeta *clonemanager.Meta,
	prLabels []string,
	agent metaagent.Agent[Req, Resp, CB],
	buildRequest func(*github.Issue, *changemanager.Session[PRData]) Req,
	buildCallbacks func(*gogit.Worktree, *changemanager.Session[PRData]) CB,
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
	log := clog.FromContext(ctx)

	switch res.Type {
	case githubreconciler.ResourceTypeIssue:
		return r.reconcileIssue(ctx, res, gh)
	case githubreconciler.ResourceTypePullRequest:
		return r.reconcilePullRequest(ctx, res, gh)
	default:
		log.With("type", res.Type).Warn("Unexpected resource type")
		return nil
	}
}
