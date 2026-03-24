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
	"strings"
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
	MaterializerModel  string `env:"MATERIALIZER_MODEL,default=gemini-2.5-flash"`
	MaterializerRegion string `env:"MATERIALIZER_REGION"` // Defaults to detected GCP region
}

// New constructs the issue materializer reconciler.
func New(ctx context.Context, identity string, tsf githubreconciler.TokenSourceFunc, cfg config) (githubreconciler.ReconcilerFunc, error) {
	projectID, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect project ID: %w", err)
	}

	zone, err := metadata.ZoneWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get zone from metadata: %w", err)
	}
	materializerRegion := cfg.MaterializerRegion
	if materializerRegion == "" {
		materializerRegion = zone[:strings.LastIndex(zone, "-")]
	}

	signer, err := gitsign.NewSigner(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gitsign signer: %w", err)
	}
	cloneMeta := clonemanager.NewMeta(ctx, tsf, identity, signer)

	type materializerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
		toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
			toolcall.NewEmptyToolsProvider[*Result](),
		),
	)

	clog.InfoContext(ctx, "Initializing materializer agent", "model", cfg.MaterializerModel, "region", materializerRegion)
	mat, err := newAgent(ctx, projectID, materializerRegion, cfg.MaterializerModel, tools)
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
