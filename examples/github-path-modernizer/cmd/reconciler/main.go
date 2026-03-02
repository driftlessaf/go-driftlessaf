/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub path modernizer reconciler that uses
// metaagent to apply Go modernize fixes to paths with diagnostics.
//
// This example demonstrates:
//   - Using metaagent for provider-agnostic AI execution (Gemini or Claude)
//   - Using metapathreconciler for path-based and PR-based reconciliation
//   - Running a static analyzer (Go modernize) to discover diagnostics
//   - Composing toolcall tools (Empty -> Worktree -> Finding)
//   - Creating PRs from AI-generated modernize fixes
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/gitsign"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/metapathreconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
	"cloud.google.com/go/compute/metadata"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
	"github.com/chainguard-dev/terraform-infra-common/pkg/httpmetrics"
	"github.com/chainguard-dev/terraform-infra-common/pkg/profiler"
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

var env = envconfig.MustProcess(context.Background(), &struct {
	Port         int    `env:"PORT,default=8080"`
	OctoIdentity string `env:"OCTO_IDENTITY,required"`
	MetricsPort  int    `env:"METRICS_PORT,default=2112"`
	EnablePprof  bool   `env:"ENABLE_PPROF,default=false"`

	// Model configuration
	Model       string `env:"MODEL,default=gemini-2.5-flash"`
	ModelRegion string `env:"MODEL_REGION"` // Defaults to detected GCP region
}{})

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go httpmetrics.ScrapeDiskUsage(ctx)
	profiler.SetupProfiler()
	defer httpmetrics.SetupTracer(ctx)()

	log := clog.FromContext(ctx)

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
	modelRegion := env.ModelRegion
	if modelRegion == "" {
		modelRegion = defaultRegion
	}

	// Create GitHub client cache with Octo identity
	clientCache := githubreconciler.NewClientCache(func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
		return githubreconciler.NewRepoTokenSource(ctx, env.OctoIdentity, org, repo), nil
	})

	// Create gitsign signer for signed commits
	signer, err := gitsign.NewSigner(ctx)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create gitsign signer: %v", err)
	}

	// Create the clone manager meta for caching clone managers per repo
	cloneMeta := clonemanager.NewMeta(ctx, func(ctx context.Context, owner, repo string) (oauth2.TokenSource, error) {
		return githubreconciler.NewRepoTokenSource(ctx, env.OctoIdentity, owner, repo), nil
	}, env.OctoIdentity, signer)

	// Configure agent tools and callbacks using composition: Empty -> Worktree -> Finding
	type modernizerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]
	tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
		toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
			toolcall.NewEmptyToolsProvider[*Result](),
		),
	)
	buildCallbacks := func(_ context.Context, session *changemanager.Session[metapathreconciler.PRData[*Request]], lease *clonemanager.Lease) (modernizerTools, error) {
		wt, err := lease.Repo().Worktree()
		if err != nil {
			return modernizerTools{}, fmt.Errorf("get worktree: %w", err)
		}
		return toolcall.NewFindingTools(
			toolcall.NewWorktreeTools(toolcall.EmptyTools{}, clonemanager.WorktreeCallbacks(wt)),
			session.FindingCallbacks(),
		), nil
	}

	// Create the modernizer agent
	log.With("model", env.Model, "region", modelRegion).
		Info("Initializing modernizer agent")
	agent, err := newAgent(ctx, projectID, modelRegion, env.Model, tools)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create modernizer agent: %v", err)
	}

	// Create the reconciler
	rec, err := newReconciler(
		ctx,
		env.OctoIdentity,
		agent,
		cloneMeta,
		[]string{env.OctoIdentity + "/managed"},
		buildCallbacks,
	)
	if err != nil {
		clog.FatalContextf(ctx, "failed to create reconciler: %v", err)
	}

	// Create duplex server (HTTP + gRPC on same port) with metrics and tracing
	d := duplex.New(
		env.Port,
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
	))

	// Register prometheus handle
	d.RegisterListenAndServeMetrics(env.MetricsPort, env.EnablePprof)

	// Register health check handler
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	// Start the server
	log.With("port", env.Port).Info("Starting modernizer reconciler")
	if err := d.ListenAndServe(ctx); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}
