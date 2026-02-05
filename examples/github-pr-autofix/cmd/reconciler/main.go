/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub PR validator reconciler with agentic fix capabilities.
// This extends the hello-world example by adding Claude-based auto-fixing of PR titles and descriptions.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
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

	// Agent configuration
	EnableAutofix  bool   `env:"ENABLE_AUTOFIX,default=false"`
	AutofixLabel   string `env:"AUTOFIX_LABEL,default=driftlessaf/autofix"`
	GCPProjectID   string `env:"GCP_PROJECT_ID"`
	GCPRegion      string `env:"GCP_REGION,default=us-central1"`
	ClaudeModel    string `env:"CLAUDE_MODEL,default=claude-sonnet-4-5@20250929"`
	MaxFixAttempts int    `env:"MAX_FIX_ATTEMPTS,default=2"`
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

	// Initialize agent if autofix is enabled
	var agent claudeexecutor.Interface[*PRContext, *PRFixResult]
	if cfg.EnableAutofix {
		if cfg.GCPProjectID == "" {
			clog.FatalContextf(ctx, "GCP_PROJECT_ID is required when ENABLE_AUTOFIX=true")
		}
		agent, err = newPRFixerAgent(ctx, &cfg)
		if err != nil {
			clog.FatalContextf(ctx, "creating agent: %v", err)
		}
		clog.InfoContextf(ctx, "Agent enabled with model %s", cfg.ClaudeModel)
	}

	clog.InfoContextf(ctx, "Using OctoSTS authentication with identity: %s", cfg.OctoIdentity)
	clientCache := githubreconciler.NewClientCache(func(ctx context.Context, org, repo string) (oauth2.TokenSource, error) {
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
		githubreconciler.WithReconciler(newReconciler(sm, &cfg, agent)),
	))

	d.RegisterListenAndServeMetrics(cfg.MetricsPort, cfg.EnablePprof)
	healthgrpc.RegisterHealthServer(d.Server, health.NewServer())

	clog.InfoContextf(ctx, "Starting PR Agent reconciler on port %d (autofix=%v)", cfg.Port, cfg.EnableAutofix)
	if err := d.ListenAndServe(ctx); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
	}
}

// newReconciler returns a reconciler function that uses the status manager.
func newReconciler(sm *statusmanager.StatusManager[prvalidation.Details], cfg *config, agent claudeexecutor.Interface[*PRContext, *PRFixResult]) githubreconciler.ReconcilerFunc {
	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm, cfg, agent)
	}
}

// hasLabel checks if the PR has a specific label.
func hasLabel(pr *github.PullRequest, labelName string) bool {
	for _, label := range pr.Labels {
		if label.GetName() == labelName {
			return true
		}
	}
	return false
}

// reconcilePR validates a PR and optionally uses an agent to fix issues.
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[prvalidation.Details], cfg *config, agent claudeexecutor.Interface[*PRContext, *PRFixResult]) error {
	log := clog.FromContext(ctx)
	log.Infof("Validating PR: %s/%s#%d", res.Owner, res.Repo, res.Number)

	// Step 1: Fetch current PR state
	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetching PR: %w", err)
	}

	// Skip closed PRs
	if pr.GetState() == "closed" {
		log.Info("Skipping closed PR")
		return nil
	}

	sha := pr.GetHead().GetSHA()
	title := pr.GetTitle()
	body := pr.GetBody()
	generation := prvalidation.ComputeGeneration(sha, title, body)

	session := sm.NewSession(gh, res, sha)

	// Check idempotency
	// Note: Generation intentionally excludes label state. Generation represents PR content
	// (what needs validating), while the label is a processing directive (how to process).
	// Including label in generation would reset fix attempts on label toggle, which could
	// be abused. Instead, we explicitly handle the "label added" case below.
	observed, err := session.ObservedState(ctx)
	if err != nil {
		return fmt.Errorf("getting observed state: %w", err)
	}
	hasAutofixLabel := hasLabel(pr, cfg.AutofixLabel)
	if observed != nil && observed.Status == "completed" && observed.Details.Generation == generation {
		// Re-run if label was added since last run (agent wasn't enabled before but label is now present).
		// This allows users to request agent assistance after seeing validation failures.
		if observed.Details.AgentEnabled || !hasAutofixLabel {
			log.Infof("Already processed generation %s, skipping", generation[:8])
			return nil
		}
		log.Infof("Label '%s' added since last run, re-processing", cfg.AutofixLabel)
	}

	// Validate PR
	titleValid, descValid, issues := prvalidation.ValidatePR(title, body)

	// If valid, set success
	if len(issues) == 0 {
		return session.SetActualState(ctx, "All checks passed!", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "success",
			Details:    prvalidation.Details{Generation: generation, TitleValid: true, DescriptionValid: true},
		})
	}

	// If autofix disabled or no agent, set failure
	if !cfg.EnableAutofix || agent == nil {
		return session.SetActualState(ctx, fmt.Sprintf("Found %d issue(s)", len(issues)), &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details:    prvalidation.Details{Generation: generation, TitleValid: titleValid, DescriptionValid: descValid, Issues: issues},
		})
	}

	// Check if the autofix label is present
	if !hasAutofixLabel {
		log.Infof("Skipping agent - '%s' label not present", cfg.AutofixLabel)
		return session.SetActualState(ctx, fmt.Sprintf("Found %d issue(s) - add '%s' label to auto-fix", len(issues), cfg.AutofixLabel), &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details:    prvalidation.Details{Generation: generation, TitleValid: titleValid, DescriptionValid: descValid, Issues: issues, AgentEnabled: false},
		})
	}

	// Check fix attempts limit
	// Only carry over fix attempts if generation matches (same PR content)
	// If generation changed (title/body was modified), reset attempts
	fixAttempts := 0
	if observed != nil && observed.Details.Generation == generation {
		fixAttempts = observed.Details.FixAttempts
	}
	if fixAttempts >= cfg.MaxFixAttempts {
		log.Infof("Max fix attempts (%d) reached, failing", cfg.MaxFixAttempts)
		return session.SetActualState(ctx, "Max fix attempts reached", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details: prvalidation.Details{
				Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
				Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts, ModelUsed: cfg.ClaudeModel,
				AgentReasoning: "Maximum fix attempts reached without successful resolution",
			},
		})
	}

	// Set in_progress status
	if err := session.SetActualState(ctx, "Agent fixing issues...", &statusmanager.Status[prvalidation.Details]{
		Status:  "in_progress",
		Details: prvalidation.Details{Generation: generation, Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts + 1, ModelUsed: cfg.ClaudeModel},
	}); err != nil {
		return fmt.Errorf("setting in_progress status: %w", err)
	}

	// Fetch changed files to give agent more context
	changedFiles, err := getChangedFiles(ctx, gh, res.Owner, res.Repo, res.Number)
	if err != nil {
		log.With("error", err).Warn("Failed to fetch changed files, continuing without them")
		changedFiles = nil
	}

	// Run agent
	prContext := &PRContext{Owner: res.Owner, Repo: res.Repo, PRNumber: res.Number, Title: title, Body: body, Issues: issues, ChangedFiles: changedFiles}
	tools := createTools(ctx, gh, res.Owner, res.Repo, res.Number)
	result, err := agent.Execute(ctx, prContext, tools)
	if err != nil {
		log.With("error", err).Error("Agent execution failed")
		return session.SetActualState(ctx, "Agent failed", &statusmanager.Status[prvalidation.Details]{
			Status:     "completed",
			Conclusion: "failure",
			Details: prvalidation.Details{
				Generation: generation, TitleValid: titleValid, DescriptionValid: descValid,
				Issues: issues, AgentEnabled: true, FixAttempts: fixAttempts + 1, ModelUsed: cfg.ClaudeModel,
				AgentReasoning: fmt.Sprintf("Agent execution error: %v", err),
			},
		})
	}

	// Re-fetch and re-validate
	pr, _, err = gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("re-fetching PR after agent: %w", err)
	}

	newTitle := pr.GetTitle()
	newBody := pr.GetBody()
	newTitleValid, newDescValid, newIssues := prvalidation.ValidatePR(newTitle, newBody)
	newGeneration := prvalidation.ComputeGeneration(sha, newTitle, newBody)

	conclusion := "success"
	summary := "All checks passed!"
	if len(newIssues) > 0 {
		conclusion = "failure"
		summary = fmt.Sprintf("Found %d issue(s) after agent fixes", len(newIssues))
	}

	return session.SetActualState(ctx, summary, &statusmanager.Status[prvalidation.Details]{
		Status:     "completed",
		Conclusion: conclusion,
		Details: prvalidation.Details{
			Generation: newGeneration, TitleValid: newTitleValid, DescriptionValid: newDescValid,
			Issues: newIssues, AgentEnabled: true, FixesApplied: result.FixesApplied,
			AgentReasoning: result.Reasoning, FixAttempts: fixAttempts + 1, ModelUsed: cfg.ClaudeModel,
		},
	})
}

// getChangedFiles fetches the list of files changed in the PR.
// This gives the agent context to generate better titles/descriptions.
func getChangedFiles(ctx context.Context, gh *github.Client, owner, repo string, prNumber int) ([]string, error) {
	files, _, err := gh.PullRequests.ListFiles(ctx, owner, repo, prNumber, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, err
	}
	var filenames []string
	for _, f := range files {
		filenames = append(filenames, f.GetFilename())
	}
	return filenames, nil
}
