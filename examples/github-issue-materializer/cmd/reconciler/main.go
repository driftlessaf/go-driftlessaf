/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub issue materializer reconciler that uses
// metaagent to transform issue problem statements into pull requests.
package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metareconciler"
	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
)

type config struct {
	// Materializer model configuration
	MaterializerModel  string `env:"MATERIALIZER_MODEL,default=gemini-3.8-flash"`
	MaterializerRegion string `env:"MATERIALIZER_REGION,default=global"` // Gemini 3.x serves only on the global endpoint
}

// New constructs the issue materializer reconciler.
func New(ctx context.Context, identity string, cc *githubreconciler.ClientCache, cfg config) (githubreconciler.ReconcilerFunc, error) {
	projectID, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect project ID: %w", err)
	}

	signer, err := gitsign.NewSigner(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gitsign signer: %w", err)
	}
	cloneMeta := clonemanager.NewMeta(ctx, cc.TokenSourceFor, identity, signer)

	type materializerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
		toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
			toolcall.NewEmptyToolsProvider[*Result](),
		),
	)

	clog.InfoContext(ctx, "Initializing materializer agent", "model", cfg.MaterializerModel, "region", cfg.MaterializerRegion)
	mat, err := newAgent(ctx, projectID, cfg.MaterializerRegion, cfg.MaterializerModel, tools)
	if err != nil {
		return nil, fmt.Errorf("create materializer: %w", err)
	}

	rec, err := newReconciler(
		identity,
		mat,
		cloneMeta,
		[]string{identity + "/managed"},
		func(_ context.Context, session *changemanager.Session[metareconciler.PRData[*Request]], lease *clonemanager.Lease) (materializerTools, error) {
			wt, err := lease.Repo().Worktree()
			if err != nil {
				return materializerTools{}, err
			}
			return toolcall.NewFindingTools(
				toolcall.NewWorktreeTools(toolcall.EmptyTools{}, clonemanager.WorktreeCallbacks(wt)),
				session.FindingCallbacks(),
			), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create reconciler: %w", err)
	}
	return rec.Reconcile, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := githubreconciler.OrgMain(ctx, New); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}
