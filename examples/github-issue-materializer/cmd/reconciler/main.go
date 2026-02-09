/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub issue materializer reconciler that uses
// metaagent to transform issue problem statements into pull requests.
//
// This example demonstrates:
//   - Using metaagent for provider-agnostic AI execution (Gemini or Claude)
//   - Using metareconciler for GitHub issue-based reconciliation
//   - Composing toolcall tools (Empty -> Worktree -> Finding)
//   - Creating PRs from AI-generated code changes
package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metareconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/chainguard-dev/terraform-infra-common/pkg/profiler"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v75/github"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

type config struct {
	Port         int    `env:"PORT,default=8080"`
	OctoIdentity string `env:"OCTO_IDENTITY,required"`
	MetricsPort  int    `env:"METRICS_PORT,default=2112"`
	EnablePprof  bool   `env:"ENABLE_PPROF,default=false"`

	// Materializer model configuration
	MaterializerModel  string `env:"MATERIALIZER_MODEL,default=gemini-2.5-flash"`
	MaterializerRegion string `env:"MATERIALIZER_REGION"` // Defaults to detected GCP region
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go httpmetrics.ScrapeDiskUsage(ctx)
	profiler.SetupProfiler()
	defer httpmetrics.SetupTracer(ctx)()

	log := clog.FromContext(ctx)

	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		clog.FatalContextf(ctx, "failed to process config: %v", err)
	}

	// Detect project ID from GCP metadata
	projectID, err := metadata.ProjectIDWithContext(ctx)
	if err != nil {
		clog.FatalContextf(ctx, "failed to detect project ID: %v", err)
	}
	log.With("project_id", projectID).Info("Detected Google Cloud project")

	// Get zone from metadata and extract region for default model region
	zone, err := metadata.ZoneWithContext(ctx)
	if err != nil {
		clog.FatalContextf(ctx, "failed to get zone from metadata: %v", err)
	}
	defaultRegion := zone[:strings.LastIndex(zone, "-")]
	log.With("region", defaultRegion).Info("Detected Google Cloud region")

	// Use configured region or fall back to detected region
	materializerRegion := cfg.MaterializerRegion
	if materializerRegion == "" {
		materializerRegion = defaultRegion
	}

	// Create GitHub client cache with Octo identity
	clientCache := githubreconciler.NewClientCache(func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
		return githubreconciler.NewOrgTokenSource(ctx, cfg.OctoIdentity, org), nil
	})

	// Create gitsign signer for signed commits
	signer, err := gitsign.NewSigner(ctx)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create gitsign signer: %v", err)
	}

	// Create the clone manager meta for caching clone managers per repo
	cloneMeta := clonemanager.NewMeta(ctx, func(ctx context.Context, owner, repo string) (oauth2.TokenSource, error) {
		return githubreconciler.NewOrgTokenSource(ctx, cfg.OctoIdentity, owner), nil
	}, cfg.OctoIdentity, signer)

	// Configure agent tools and callbacks using composition: Empty -> Worktree -> Finding
	type materializerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
		toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
			toolcall.NewEmptyToolsProvider[*Result](),
		),
	)
	buildCallbacks := func(wt *gogit.Worktree, session *changemanager.Session[metareconciler.PRData]) materializerTools {
		return toolcall.NewFindingTools(
			toolcall.NewWorktreeTools(toolcall.EmptyTools{}, clonemanager.WorktreeCallbacks(wt)),
			session.FindingCallbacks(),
		)
	}

	// Create the materializer agent
	log.With("model", cfg.MaterializerModel, "region", materializerRegion).
		Info("Initializing materializer agent")
	mat, err := newAgent(ctx, projectID, materializerRegion, cfg.MaterializerModel, tools)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create materializer: %v", err)
	}

	// Create the reconciler
	rec, err := newReconciler(
		cfg.OctoIdentity,
		mat,
		cloneMeta,
		[]string{cfg.OctoIdentity + "/managed"},
		buildCallbacks,
	)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create reconciler: %v", err)
	}

	// Create duplex server (HTTP + gRPC on same port) with metrics and tracing
	d := duplex.New(
		cfg.Port,
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainStreamInterceptor(kmetrics.StreamServerInterceptor()),
		grpc.ChainUnaryInterceptor(
			kmetrics.UnaryServerInterceptor(),
			recovery.UnaryServerInterceptor(),
		),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	// Register workqueue service with the GitHub reconciler
	workqueue.RegisterWorkqueueServiceServer(d.Server, githubreconciler.NewReconciler(
		clientCache,
		githubreconciler.WithReconciler(githubreconciler.ReconcilerFunc(func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
			return rec.Reconcile(ctx, res, gh)
		})),
		githubreconciler.WithOrgScopedCredentials(),
	))

	// Register prometheus handle
	d.RegisterListenAndServeMetrics(cfg.MetricsPort, cfg.EnablePprof)

	// Register health check handler
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	// Start the server
	log.With("port", cfg.Port).Info("Starting materializer reconciler")
	if err := d.ListenAndServe(ctx); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}
