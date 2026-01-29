/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package main implements a GitHub PR validator reconciler.
// This demonstrates the DriftlessAF pattern for validating PRs and creating check runs.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

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

// Conventional commit prefixes
var conventionalPrefixes = []string{
	"feat", "fix", "docs", "style", "refactor",
	"perf", "test", "build", "ci", "chore", "revert",
}

// conventionalCommitRegex matches titles like "feat: add new feature" or "fix(scope): bug fix"
var conventionalCommitRegex = regexp.MustCompile(`^(` + strings.Join(conventionalPrefixes, "|") + `)(\(.+\))?:\s+.+`)

type config struct {
	Port        int  `env:"PORT,default=8080"`
	MetricsPort int  `env:"METRICS_PORT,default=2112"`
	EnablePprof bool `env:"ENABLE_PPROF,default=false"`

	// OctoSTS identity for GitHub authentication
	OctoIdentity string `env:"OCTO_IDENTITY,required"`
}

// Details holds validation-specific state for the status manager.
// This is persisted in the check run and can be retrieved on subsequent reconciliations.
type Details struct {
	// Generation is a hash of SHA + title + body for idempotency.
	// This allows re-validation when PR metadata changes, not just code.
	// Following the pattern from qackage's ElasticBuildChecker.
	Generation       string   `json:"generation"`
	TitleValid       bool     `json:"titleValid"`
	DescriptionValid bool     `json:"descriptionValid"`
	Issues           []string `json:"issues,omitempty"`
}

// Markdown renders the validation details as markdown for the check run output.
func (d Details) Markdown() string {
	var sb strings.Builder
	sb.WriteString("## PR Validation Report\n\n")
	sb.WriteString("| Check | Status |\n")
	sb.WriteString("|-------|--------|\n")

	titleStatus := "❌ Invalid"
	if d.TitleValid {
		titleStatus = "✅ Valid"
	}
	sb.WriteString(fmt.Sprintf("| Title (conventional commit) | %s |\n", titleStatus))

	descStatus := "❌ Invalid"
	if d.DescriptionValid {
		descStatus = "✅ Valid"
	}
	sb.WriteString(fmt.Sprintf("| Description | %s |\n", descStatus))

	if len(d.Issues) > 0 {
		sb.WriteString("\n### Issues\n\n")
		for _, issue := range d.Issues {
			sb.WriteString(issue)
			sb.WriteString("\n\n---\n\n")
		}
	}

	return sb.String()
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
	sm, err := statusmanager.NewStatusManager[Details](ctx, cfg.OctoIdentity)
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
func newReconciler(sm *statusmanager.StatusManager[Details]) githubreconciler.ReconcilerFunc {
	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm)
	}
}

// reconcilePR validates a PR and creates/updates a check run with the results.
// This demonstrates the reconciler pattern: fetch state, compute desired state, apply.
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[Details]) error {
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
	generation := computeGeneration(sha, title, body)

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
	titleValid, descValid, issues := validatePR(title, body)

	conclusion := "success"
	summary := "All checks passed!"
	if len(issues) > 0 {
		conclusion = "failure"
		summary = fmt.Sprintf("Found %d issue(s)", len(issues))
	}

	clog.InfoContextf(ctx, "Validation result: %s", conclusion)

	// Step 3: Update status via the status manager
	// Store generation in Details for idempotency (following qackage pattern)
	status := &statusmanager.Status[Details]{
		Status:     "completed",
		Conclusion: conclusion,
		Details: Details{
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

// computeGeneration creates a unique key from SHA, title, and body.
// This ensures idempotency is based on the full PR state, not just the commit.
func computeGeneration(sha, title, body string) string {
	h := sha256.New()
	h.Write([]byte(sha))
	h.Write([]byte(title))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// validatePR checks the PR title and description against our conventions.
// Returns whether title is valid, description is valid, and a list of issues.
func validatePR(title, body string) (titleValid, descValid bool, issues []string) {
	// Validate title follows conventional commit format
	titleValid = conventionalCommitRegex.MatchString(title)
	if !titleValid {
		issues = append(issues, fmt.Sprintf(
			"**Title** does not follow [conventional commit](https://www.conventionalcommits.org/) format.\n"+
				"  - Expected: `<type>: <description>` or `<type>(scope): <description>`\n"+
				"  - Valid types: `%s`\n"+
				"  - Got: `%s`",
			strings.Join(conventionalPrefixes, "`, `"),
			title,
		))
	}

	// Validate description is not empty
	trimmedBody := strings.TrimSpace(body)
	switch {
	case trimmedBody == "":
		issues = append(issues, "**Description** is empty. Please add a description explaining the changes.")
	case len(trimmedBody) < 20:
		issues = append(issues, "**Description** is too short. Please provide more context about the changes.")
	default:
		descValid = true
	}

	return titleValid, descValid, issues
}
