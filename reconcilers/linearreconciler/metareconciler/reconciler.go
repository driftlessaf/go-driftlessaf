/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"strings"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
)

// Reconciler is a generic reconciler that bridges Linear issues to GitHub PRs
// via an AI agent. It mirrors the githubreconciler/metareconciler but uses
// Linear as the issue source and GitHub as the code execution target.
//
// Type parameters:
//
//   - Req, Resp, CB: the agent's request, response, and callbacks types
//     (see metaagent.Agent)
//   - T: the bot's State value type. The bot defines a struct that
//     value-embeds metareconciler.State (so embedded methods promote and
//     satisfy StateMachine on *T) and adds bot-specific fields. For bots
//     with no extras, `T = metareconciler.State` works directly.
//   - PT: pointer-to-T constrained to satisfy StateMachine. Almost always
//     `*T` literally; the explicit pair is needed because Go generics need
//     to construct fresh instances via `var t T; pt := PT(&t)`.
type Reconciler[Req promptbuilder.Bindable, Resp Result, CB any, T any, PT StateConstraint[T]] struct {
	identity      string
	changeManager *changemanager.CM[PRData[Req]]
	cloneMeta     *clonemanager.Meta
	prLabels      []string

	// requiredLabel is checked before processing an issue. If set and the issue
	// doesn't have this label, reconciliation is skipped.
	requiredLabel string

	// upstreamPrefix is the state prefix used by the upstream bot (e.g. "planner")
	// to read repo target information from its state attachment.
	upstreamPrefix string

	// repoTargetResolver is an optional fallback for resolving repo targets
	// when no upstream bot state attachment is available.
	repoTargetResolver RepoTargetResolver

	// Agent and its adapters
	agent          metaagent.Agent[Req, Resp, CB]
	buildRequest   RequestBuilder[Req, PRData[Req]]
	buildCallbacks CallbacksBuilder[CB, PRData[Req]]

	// Clients
	linearClient  *linearreconciler.Client
	githubClients *githubreconciler.ClientCache

	// saveCallback is invoked after every successful StateManager save.
	// Optional; nil means no callback. Set via WithSaveCallback. Threaded
	// through to every StateManager constructed by (*Reconciler).NewStateManager.
	saveCallback SaveCallback
}

// options collects the configurable knobs that don't depend on the
// Reconciler's type parameters. Keeping them in a separate struct lets
// Option be a plain non-generic function — call sites don't have to spell
// out [Req, Resp, CB] at every option invocation.
type options struct {
	requiredLabel      string
	upstreamPrefix     string
	repoTargetResolver RepoTargetResolver
	saveCallback       SaveCallback
}

// Option configures a Reconciler. It is intentionally non-generic; the
// settable fields don't depend on Req/Resp/CB.
type Option func(*options)

// WithRequiredLabel configures the reconciler to only process issues that have
// the specified label. Issues without this label are skipped during reconciliation.
func WithRequiredLabel(label string) Option {
	return func(o *options) {
		o.requiredLabel = label
	}
}

// WithUpstreamPrefix sets the state prefix used by the upstream bot whose
// state attachment contains the repo target. Defaults to "planner" when
// no option is supplied.
func WithUpstreamPrefix(prefix string) Option {
	return func(o *options) {
		o.upstreamPrefix = prefix
	}
}

// WithRepoTargetResolver sets a fallback resolver for determining the repo
// target from a Linear issue when no upstream bot state is available.
//
// Note: the resolver is invoked during reconcile, but it must be supplied
// at construction time — which means resolvers needing a state-machine-
// aware StateManager cannot reach for the (*Reconciler).NewStateManager
// method (it doesn't exist yet). Use the free-function form
// metareconciler.NewStateManager[T, *T](client, issue) instead.
func WithRepoTargetResolver(resolver RepoTargetResolver) Option {
	return func(o *options) {
		o.repoTargetResolver = resolver
	}
}

// WithSaveCallback registers a post-save hook that fires after every
// successful StateManager.Save. The callback receives the persisted JSON
// and (when the save included a Status or FailureMode change) the
// StateTransition that was appended to History.
//
// Threaded automatically into every StateManager constructed via
// Reconciler.NewStateManager. Callers using the free-function
// metareconciler.NewStateManager (e.g. RepoTargetResolver implementations
// constructed before the Reconciler exists) opt in by chaining
// .SetSaveCallback(cb) on the returned manager.
//
// The callback is intended for downstream mirroring and observability.
// Errors returned from it are logged at Warn but never fail the save.
func WithSaveCallback(cb SaveCallback) Option {
	return func(o *options) {
		o.saveCallback = cb
	}
}

// New creates a new Linear metareconciler. It bridges Linear issues to GitHub
// PRs by reading repo target information from an upstream bot's state attachment
// and using the agent to generate code changes.
//
// State type parameters T and PT are usually inferred from the constructor
// arguments at call sites that provide a buildRequest closure typed against
// the bot's wrapper State; for explicit construction, pass them as in:
//
//	metareconciler.New[*Req, *Resp, CB, mybot.State, *mybot.State](...)
func New[Req promptbuilder.Bindable, Resp Result, CB any, T any, PT StateConstraint[T]](
	identity string,
	changeManager *changemanager.CM[PRData[Req]],
	cloneMeta *clonemanager.Meta,
	prLabels []string,
	agent metaagent.Agent[Req, Resp, CB],
	buildRequest RequestBuilder[Req, PRData[Req]],
	buildCallbacks CallbacksBuilder[CB, PRData[Req]],
	linearClient *linearreconciler.Client,
	githubClients *githubreconciler.ClientCache,
	opts ...Option,
) *Reconciler[Req, Resp, CB, T, PT] {
	o := &options{upstreamPrefix: "planner"}
	for _, opt := range opts {
		opt(o)
	}
	return &Reconciler[Req, Resp, CB, T, PT]{
		identity:           identity,
		changeManager:      changeManager,
		cloneMeta:          cloneMeta,
		prLabels:           prLabels,
		requiredLabel:      o.requiredLabel,
		upstreamPrefix:     o.upstreamPrefix,
		repoTargetResolver: o.repoTargetResolver,
		agent:              agent,
		buildRequest:       buildRequest,
		buildCallbacks:     buildCallbacks,
		linearClient:       linearClient,
		githubClients:      githubClients,
		saveCallback:       o.saveCallback,
	}
}

// Reconcile processes a Linear issue. This method satisfies the
// linearreconciler.ReconcilerFunc signature.
//
// The `client` parameter is mandated by the ReconcilerFunc signature but
// not used here: the reconciler holds its own *Client (passed at New) for
// state-attachment operations performed in helpers (resolveRepoTarget,
// state save/load) that are called outside of this entry point. Callers
// are expected to pass the same instance.
func (r *Reconciler[Req, Resp, CB, T, PT]) Reconcile(ctx context.Context, issue *linearreconciler.Issue, _ *linearreconciler.Client) error {
	return r.reconcileIssue(ctx, issue)
}

// WrapWithPRHandler wraps a linearreconciler.Reconciler with GitHub PR event
// handling. The returned server processes both Linear issue UUIDs and GitHub
// PR URLs as workqueue keys:
//   - UUID keys are handled by the linear reconciler (standard flow)
//   - GitHub PR URLs trigger re-queuing of the linked Linear issue ID
//     (extracted from PRData embedded in the PR body)
//
// This enables the CI feedback loop: PR CI fails → check_suite event →
// PR URL queued → extract Linear issue ID → re-queue → iterate.
func (r *Reconciler[Req, Resp, CB, T, PT]) WrapWithPRHandler(linearRec workqueue.WorkqueueServiceServer) workqueue.WorkqueueServiceServer {
	return &dualKeyServer{
		metaRec:   r,
		linearRec: linearRec,
	}
}

// prEventHandler abstracts the bit of Reconciler that dualKeyServer needs.
// Defined as an interface so the routing logic can be tested without
// constructing a fully-wired Reconciler[Req, Resp, CB].
type prEventHandler interface {
	HandlePREvent(ctx context.Context, prURL string) (*workqueue.ProcessResponse, error)
}

// dualKeyServer wraps a linear reconciler to handle both Linear issue UUIDs
// and GitHub PR URLs as workqueue keys.
type dualKeyServer struct {
	workqueue.UnimplementedWorkqueueServiceServer
	metaRec   prEventHandler
	linearRec workqueue.WorkqueueServiceServer
}

func (d *dualKeyServer) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	// ParseURL accepts issues, commits, trees, etc. as well as PRs. Branch
	// explicitly so we can (a) only re-queue Linear issues for actual PR
	// events, and (b) skip non-PR GitHub URLs early — passing them on to
	// the Linear reconciler would surface as opaque "issue not found"
	// errors from the Linear API.
	if res, err := githubreconciler.ParseURL(req.Key); err == nil {
		if res.Type == githubreconciler.ResourceTypePullRequest {
			clog.InfoContextf(ctx, "Processing GitHub PR event: %s", req.Key)
			return d.metaRec.HandlePREvent(ctx, req.Key)
		}
		clog.InfoContextf(ctx, "Ignoring non-PR GitHub URL: %s (type: %v)", req.Key, res.Type)
		return &workqueue.ProcessResponse{}, nil
	}
	// ParseURL doesn't recognise every GitHub URL shape (commits, releases,
	// tree views). Drop them rather than handing off to the Linear
	// reconciler, which would surface as an obscure "issue not found".
	if strings.HasPrefix(req.Key, "https://github.com/") {
		clog.InfoContextf(ctx, "Ignoring unrecognised GitHub URL: %s", req.Key)
		return &workqueue.ProcessResponse{}, nil
	}
	return d.linearRec.Process(ctx, req)
}
