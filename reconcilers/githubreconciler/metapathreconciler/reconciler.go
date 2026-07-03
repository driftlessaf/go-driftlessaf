/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
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
	// .{identity}.yaml config file at the repository root.
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

// PRReconciler is a generic reconciler for metaagent-based path handlers
// that remediates findings by creating and iterating on pull requests.
type PRReconciler[Req promptbuilder.Bindable, Resp Result, CB any] struct {
	core

	changeManager *changemanager.CM[PRData[Req]]

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

	// giveUp, when set, surfaces an agent's deliberate no-op explanation on the
	// PR as a single marker comment. Nil is a safe no-op receiver. See
	// WithGiveUpComment.
	giveUp *changemanager.GiveUpComment
}

// NewPR creates a generic metaagent path reconciler that remediates
// analyzer findings by creating and iterating on pull requests.
func NewPR[Req promptbuilder.Bindable, Resp Result, CB any](
	ctx context.Context,
	identity string,
	analyzer Analyzer,
	changeManager *changemanager.CM[PRData[Req]],
	cloneMeta *clonemanager.Meta,
	agent metaagent.Agent[Req, Resp, CB],
	buildRequest func(context.Context, *changemanager.Session[PRData[Req]], *gogit.Worktree, []callbacks.Finding) (Req, error),
	buildCallbacks func(context.Context, *changemanager.Session[PRData[Req]], *clonemanager.Lease) (CB, error),
	opts ...PROption,
) (*PRReconciler[Req, Resp, CB], error) {
	if analyzer == nil {
		return nil, errors.New("analyzer must be provided")
	}

	o := prOptions{commonOptions: commonOptions{mode: ModeConfig}}
	for _, opt := range opts {
		opt.applyPR(&o)
	}

	clog.InfoContext(ctx, "Starting metapathreconciler", "mode", o.mode)

	c, err := newCore(ctx, identity, analyzer, cloneMeta, o.commonOptions)
	if err != nil {
		return nil, err
	}
	return &PRReconciler[Req, Resp, CB]{
		core:                            c,
		changeManager:                   changeManager,
		baseRevalidation:                o.baseRevalidation,
		unknownMergeabilityRequeueAfter: o.unknownMergeabilityRequeueAfter,
		agent:                           agent,
		buildRequest:                    buildRequest,
		buildCallbacks:                  buildCallbacks,
		giveUp:                          o.giveUp,
	}, nil
}

// Reconcile processes a path or pull request URL.
// For paths: runs the analyzer and agent to create/update a PR.
// For PRs: extracts the original path from the branch name and queues it.
func (r *PRReconciler[Req, Resp, CB]) Reconcile(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
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
