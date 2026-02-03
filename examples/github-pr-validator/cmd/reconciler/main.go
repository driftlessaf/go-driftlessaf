/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub PR validator using the DriftlessAF reconciler pattern.
// Validates PR titles (conventional commits) and descriptions, reporting results via check runs.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"chainguard.dev/driftlessaf/examples/prvalidation"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"chainguard.dev/driftlessaf/workqueue"
	"chainguard.dev/go-grpc-kit/pkg/duplex"
	kmetrics "chainguard.dev/go-grpc-kit/pkg/metrics"
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

type config struct {
	Port        int  `env:"PORT,default=8080"`
	MetricsPort int  `env:"METRICS_PORT,default=2112"`
	EnablePprof bool `env:"ENABLE_PPROF,default=false"`

	// OctoSTS identity for GitHub authentication
	OctoIdentity string `env:"OCTO_IDENTITY,required"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go httpmetrics.ScrapeDiskUsage(ctx)
	profiler.SetupProfiler()
	defer httpmetrics.SetupTracer(ctx)()

	var cfg config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		clog.FatalContextf(ctx, "processing config: %v", err)
	}

	// Create the status manager for check run management
	sm, err := statusmanager.NewStatusManager[prvalidation.Details](ctx, cfg.OctoIdentity)
	if err != nil {
		clog.FatalContextf(ctx, "creating status manager: %v", err)
	}

	clog.InfoContextf(ctx, "Using OctoSTS authentication with identity: %s", cfg.OctoIdentity)
	clientCache := githubreconciler.NewClientCache(func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
		// Always use org-scoped tokens - policy is in {org}/.github repo
		clog.InfoContextf(ctx, "OctoSTS: requesting org-scoped token for identity=%q org=%q (repo=%q)", cfg.OctoIdentity, org, repo)
		return githubreconciler.NewOrgTokenSource(ctx, cfg.OctoIdentity, org), nil
	})

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

	// Register the GitHub reconciler with PR validation logic
	workqueue.RegisterWorkqueueServiceServer(d.Server, githubreconciler.NewReconciler(
		clientCache,
		githubreconciler.WithReconciler(newReconciler(sm)),
	))

	d.RegisterListenAndServeMetrics(cfg.MetricsPort, cfg.EnablePprof)
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	clog.InfoContextf(ctx, "Starting PR Validator reconciler on port %d", cfg.Port)
	if err := d.ListenAndServe(ctx); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}

// newReconciler returns a reconciler function that uses the status manager.
func newReconciler(sm *statusmanager.StatusManager[prvalidation.Details]) githubreconciler.ReconcilerFunc {
	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm)
	}
}

// reconcilePR validates a PR and creates/updates a check run with the results.
// This demonstrates the reconciler pattern: fetch state, compute desired state, apply.
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[prvalidation.Details]) error {
	clog.InfoContextf(ctx, "Validating PR: %s/%s#%d", res.Owner, res.Repo, res.Number)

	// Step 1: Fetch current PR state
	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetching PR: %w", err)
	}

	// Skip closed PRs - no need to validate
	if pr.GetState() == "closed" {
		clog.InfoContextf(ctx, "Skipping closed PR")
		return nil
	}

	// Get the commit SHA for the check run (status is attached to the commit)
	sha := pr.GetHead().GetSHA()

	// Step 2: Get PR title and description for validation
	title := pr.GetTitle()
	body := pr.GetBody()

	// Compute generation key from SHA + title + body
	// This ensures we re-validate when PR metadata changes, not just code
	generation := prvalidation.ComputeGeneration(sha, title, body)

	session := sm.NewSession(gh, res, sha)

	// Check if we've already processed this exact state (idempotency)
	// Following the qackage pattern: check Details.Generation, not ObservedGeneration
	// because statusmanager always sets ObservedGeneration to SHA
	observed, err := session.ObservedState(ctx)
	if err != nil {
		return fmt.Errorf("getting observed state: %w", err)
	}
	if observed != nil && observed.Status == "completed" && observed.Details.Generation == generation {
		clog.InfoContextf(ctx, "Already processed generation %s, skipping", generation[:8])
		return nil
	}
	titleValid, descValid, issues := prvalidation.ValidatePR(title, body)

	conclusion := "success"
	summary := "All checks passed!"
	if len(issues) > 0 {
		conclusion = "failure"
		summary = fmt.Sprintf("Found %d issue(s)", len(issues))
	}

	clog.InfoContextf(ctx, "Validation result: %s", conclusion)

	// Step 3: Update status via the status manager
	// Store generation in Details for idempotency (following qackage pattern)
	status := &statusmanager.Status[prvalidation.Details]{
		Status:     "completed",
		Conclusion: conclusion,
		Details: prvalidation.Details{
			Generation:       generation,
			TitleValid:       titleValid,
			DescriptionValid: descValid,
			Issues:           issues,
		},
	}

	if err := session.SetActualState(ctx, summary, status); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}

	return nil
}
