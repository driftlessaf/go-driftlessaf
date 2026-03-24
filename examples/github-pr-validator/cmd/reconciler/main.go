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
	"os/signal"
	"syscall"

	"chainguard.dev/driftlessaf/examples/prvalidation"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"github.com/chainguard-dev/clog"
	_ "github.com/chainguard-dev/clog/gcp/init"
	"github.com/google/go-github/v84/github"
)

type config struct{}

func New(ctx context.Context, identity string, _ githubreconciler.TokenSourceFunc, _ config) (githubreconciler.ReconcilerFunc, error) {
	sm, err := statusmanager.NewStatusManager[prvalidation.Details](ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("creating status manager: %w", err)
	}
	return func(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
		return reconcilePR(ctx, res, gh, sm)
	}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := githubreconciler.RepoMain(ctx, New); err != nil {
		clog.FatalContextf(ctx, "server failed: %v", err)
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
	return session.SetActualState(ctx, summary, &statusmanager.Status[prvalidation.Details]{
		Status:     "completed",
		Conclusion: conclusion,
		Details: prvalidation.Details{
			Generation:       generation,
			TitleValid:       titleValid,
			DescriptionValid: descValid,
			Issues:           issues,
		},
	})
}
