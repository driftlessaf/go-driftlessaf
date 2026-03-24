/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub path modernizer reconciler that uses
// metaagent to apply Go modernize fixes to paths with diagnostics.
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
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metapathreconciler"
	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
)

type config struct {
	// Model configuration
	Model       string `env:"MODEL,default=gemini-2.5-flash"`
	ModelRegion string `env:"MODEL_REGION"` // Defaults to detected GCP region
}

// New constructs the path modernizer reconciler.
func New(ctx context.Context, identity string, tsf githubreconciler.TokenSourceFunc, cfg config) (githubreconciler.ReconcilerFunc, error) {
	projectID, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect project ID: %w", err)
	}

	zone, err := metadata.ZoneWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get zone from metadata: %w", err)
	}
	modelRegion := cfg.ModelRegion
	if modelRegion == "" {
		modelRegion = zone[:strings.LastIndex(zone, "-")]
	}

	signer, err := gitsign.NewSigner(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gitsign signer: %w", err)
	}
	cloneMeta := clonemanager.NewMeta(ctx, tsf, identity, signer)

	type modernizerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
		toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
			toolcall.NewEmptyToolsProvider[*Result](),
		),
	)

	clog.InfoContext(ctx, "Initializing modernizer agent", "model", cfg.Model, "region", modelRegion)
	agent, err := newAgent(ctx, projectID, modelRegion, cfg.Model, tools)
	if err != nil {
		return nil, fmt.Errorf("create modernizer agent: %w", err)
	}

	rec, err := newReconciler(
		ctx,
		identity,
		agent,
		cloneMeta,
		[]string{identity + "/managed"},
		func(_ context.Context, session *changemanager.Session[metapathreconciler.PRData[*Request]], lease *clonemanager.Lease) (modernizerTools, error) {
			wt, err := lease.Repo().Worktree()
			if err != nil {
				return modernizerTools{}, fmt.Errorf("get worktree: %w", err)
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

	if err := githubreconciler.RepoMain(ctx, New); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}
