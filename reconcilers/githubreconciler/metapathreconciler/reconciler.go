/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
)

// Mode controls which behaviors the reconciler performs.
// Modes can be combined with bitwise OR.
type Mode int

const (
	// ModeFix handles paths and own PRs.
	ModeFix Mode = 1 << iota
	// ModeReview reviews other PRs.
	ModeReview
	// ModeConfig delegates all behavior decisions to the per-repo
	// .github/chainguard/{identity}.yaml config file.
	ModeConfig
	// ModeNone disables all behaviors.
	ModeNone Mode = 0
	// ModeAll handles paths, own PRs, and reviews other PRs.
	ModeAll = ModeFix | ModeReview
)

// EnvDecode implements github.com/sethvargo/go-envconfig.Decoder so Mode
// can be used directly in envconfig structs. Valid values: fix, review, all, none, config.
func (m *Mode) EnvDecode(val string) error {
	switch strings.TrimSpace(strings.ToLower(val)) {
	case "fix":
		*m = ModeFix
	case "review":
		*m = ModeReview
	case "all":
		*m = ModeAll
	case "none":
		*m = ModeNone
	case "config":
		*m = ModeConfig
	default:
		return fmt.Errorf("unknown mode %q", val)
	}
	return nil
}

// String returns a human-readable representation of the mode.
func (m Mode) String() string {
	switch m {
	case ModeAll:
		return "all"
	case ModeFix:
		return "fix"
	case ModeReview:
		return "review"
	case ModeConfig:
		return "config"
	case ModeNone:
		return "none"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// ShouldFix reports whether m includes fix behavior.
func (m Mode) ShouldFix() bool { return m&ModeFix != 0 }

// ShouldReview reports whether m includes review behavior.
func (m Mode) ShouldReview() bool { return m&ModeReview != 0 }

// IsConfig reports whether m delegates behavior to the per-repo config file.
func (m Mode) IsConfig() bool { return m&ModeConfig != 0 }

// Reconciler is a generic reconciler for metaagent-based path handlers.
type Reconciler[Req promptbuilder.Bindable, Resp Result, CB any] struct {
	identity      string
	analyzer      Analyzer
	statusManager *statusmanager.StatusManager[CheckDetails]
	changeManager *changemanager.CM[PRData[Req]]
	cloneMeta     *clonemanager.Meta
	prLabels      []string
	mode          Mode

	// baseRevalidation, when true, makes iteration passes re-run the analyzer
	// against the base branch (and the PR branch) before iterating, closing
	// PRs whose update has already landed and refreshing superseded PRs with
	// the newest update. See WithBaseRevalidation.
	baseRevalidation bool

	// unknownMergeabilityRequeueAfter, when > 0, requeues a PR whose mergeability
	// GitHub has not yet computed instead of resetting it from the default
	// branch. Zero keeps the historical behavior. See
	// WithRequeueOnUnknownMergeability.
	unknownMergeabilityRequeueAfter time.Duration

	// Agent and its adapters
	agent          metaagent.Agent[Req, Resp, CB]
	buildRequest   func(context.Context, *changemanager.Session[PRData[Req]], *gogit.Worktree, []callbacks.Finding) (Req, error)
	buildCallbacks func(context.Context, *changemanager.Session[PRData[Req]], *clonemanager.Lease) (CB, error)

	// labelFn optionally computes additional PR labels from diagnostics/findings.
	labelFn func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string

	// giveUp, when set, surfaces an agent's deliberate no-op explanation on the
	// PR as a single marker comment. Nil is a safe no-op receiver. See
	// WithGiveUpComment.
	giveUp *changemanager.GiveUpComment
}

// Option configures a Reconciler.
type Option func(*option)

type option struct {
	mode                            Mode
	labelFn                         func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string
	baseRevalidation                bool
	unknownMergeabilityRequeueAfter time.Duration
	giveUp                          *changemanager.GiveUpComment
}

// WithMode configures the reconciler's operating mode.
func WithMode(m Mode) Option {
	return func(o *option) {
		o.mode = m
	}
}

// WithLabelFunc configures a function that computes additional PR labels
// based on analyzer diagnostics and/or CI findings. The returned labels
// are merged with the static prLabels passed to New.
//
// On the first pass (analyzer runs), diagnostics is populated and findings
// contains unfixed diagnostics converted to findings.
// On iteration passes (PR has CI failures), diagnostics is nil and findings
// contains the session's CI/review findings.
func WithLabelFunc(fn func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string) Option {
	return func(o *option) {
		o.labelFn = fn
	}
}

// WithBaseRevalidation makes the reconciler re-run the analyzer against the
// base branch before iterating on a PR with CI findings, closing PRs whose
// update has already landed and refreshing PRs superseded by a newer version
// with the newest update (see needsRefresh). Off by default to avoid the
// extra analyzer runs for reconcilers that do not need it.
func WithBaseRevalidation() Option {
	return func(o *option) {
		o.baseRevalidation = true
	}
}

// WithRequeueOnUnknownMergeability requeues after the given delay when GitHub
// has not yet computed a PR's mergeability and there is nothing else to act on
// (no findings, no pending checks), instead of resetting the PR from the
// default branch. A non-positive delay disables it (the default).
func WithRequeueOnUnknownMergeability(after time.Duration) Option {
	return func(o *option) {
		o.unknownMergeabilityRequeueAfter = after
	}
}

// WithGiveUpComment surfaces an agent's deliberate no-op on the PR. When the
// agent runs but makes no file changes and its result implements
// changemanager.Explainer with a non-empty explanation, the reconciler upserts
// a single comment (identified by marker) whose body is render(explanation).
// Repeated identical give-ups rewrite nothing, and the comment is cleared when
// the PR recovers. Off by default: reconcilers that do not set this keep the
// silent no-change behavior.
func WithGiveUpComment(marker string, render func(explanation string) string) Option {
	return func(o *option) {
		o.giveUp = &changemanager.GiveUpComment{Marker: marker, Render: render}
	}
}

// New creates a new generic metaagent path reconciler.
func New[Req promptbuilder.Bindable, Resp Result, CB any](
	ctx context.Context,
	identity string,
	analyzer Analyzer,
	changeManager *changemanager.CM[PRData[Req]],
	cloneMeta *clonemanager.Meta,
	prLabels []string,
	agent metaagent.Agent[Req, Resp, CB],
	buildRequest func(context.Context, *changemanager.Session[PRData[Req]], *gogit.Worktree, []callbacks.Finding) (Req, error),
	buildCallbacks func(context.Context, *changemanager.Session[PRData[Req]], *clonemanager.Lease) (CB, error),
	opts ...Option,
) (*Reconciler[Req, Resp, CB], error) {
	o := option{mode: ModeConfig}
	for _, opt := range opts {
		opt(&o)
	}

	clog.InfoContext(ctx, "Starting metapathreconciler", "mode", o.mode)

	sm, err := statusmanager.NewStatusManager[CheckDetails](ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("create status manager: %w", err)
	}
	return &Reconciler[Req, Resp, CB]{
		identity:                        identity,
		analyzer:                        analyzer,
		statusManager:                   sm,
		changeManager:                   changeManager,
		cloneMeta:                       cloneMeta,
		prLabels:                        prLabels,
		mode:                            o.mode,
		baseRevalidation:                o.baseRevalidation,
		unknownMergeabilityRequeueAfter: o.unknownMergeabilityRequeueAfter,
		agent:                           agent,
		buildRequest:                    buildRequest,
		buildCallbacks:                  buildCallbacks,
		labelFn:                         o.labelFn,
		giveUp:                          o.giveUp,
	}, nil
}

// Reconcile processes a path or pull request URL.
// For paths: runs the analyzer and agent to create/update a PR.
// For PRs: extracts the original path from the branch name and queues it.
func (r *Reconciler[Req, Resp, CB]) Reconcile(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	switch res.Type {
	case githubreconciler.ResourceTypePath:
		if !r.mode.ShouldFix() && !r.mode.IsConfig() {
			return nil
		}
		return r.reconcilePath(ctx, res, gh)
	case githubreconciler.ResourceTypePullRequest:
		return r.reconcilePullRequest(ctx, res, gh)
	default:
		clog.WarnContext(ctx, "Unexpected resource type", "type", res.Type)
		return nil
	}
}
