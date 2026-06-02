/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package branchreconciler_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/branchreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"github.com/go-git/go-git/v5"
)

// ExampleReconciler demonstrates creating a branch reconciler for automated
// dependency updates that iterate until tests pass.
func ExampleReconciler() {
	ctx := context.Background()

	// Mock dependencies
	var (
		tokenSourceFunc githubreconciler.TokenSourceFunc
		signer          git.Signer
	)

	// Setup
	cloneMeta := clonemanager.NewMeta(ctx, tokenSourceFunc, "update-bot", signer)
	clientCache := githubreconciler.NewClientCache(tokenSourceFunc)

	// Create reconciler for dependency updates
	rec, err := branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			// key format: "owner/repo/dependency@version"
			// Example: "myorg/myapp/lodash@4.17.21"
			parts := strings.Split(key, "/")
			if len(parts) < 3 {
				return "", "", "", fmt.Errorf("invalid key format: %s", key)
			}
			owner, repo := parts[0], parts[1]
			depInfo := strings.Join(parts[2:], "/")
			branch := "deps/" + strings.ReplaceAll(depInfo, "@", "-")
			return owner, repo, branch, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			// Update dependency and fix any breaking changes
			// This could use AI to resolve compatibility issues
			return fmt.Sprintf("Update dependency (attempt %d/%d)\n\nAuto-resolved compatibility issues",
				info.CommitCount+1, 10), nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Check if all tests pass on this commit
			// In production, this would query your CI system
			testsPassing := true // Placeholder
			return testsPassing, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Create PR for human review when tests pass
			fmt.Printf("Creating PR for %s/%s: %s → %s\n",
				info.Owner, info.Repo, info.Branch, info.BaseBranch)
			return nil
		}),
		branchreconciler.WithMaxAttempts(10),
		branchreconciler.WithRequeueDelay(5*time.Minute),
	)
	if err != nil {
		panic(err)
	}

	// Use with workqueue dispatcher
	// dispatcher.Handle(ctx, wq, concurrency, batchSize, dispatcher.ServiceCallback(rec))
	_ = rec // Reconciler is ready to be used with dispatcher
}

// ExampleReconciler_documentationGeneration demonstrates automated documentation
// generation that iterates until validation passes.
func ExampleReconciler_documentationGeneration() {
	ctx := context.Background()

	var (
		tokenSourceFunc githubreconciler.TokenSourceFunc
		signer          git.Signer
	)

	cloneMeta := clonemanager.NewMeta(ctx, tokenSourceFunc, "docs-bot", signer)
	clientCache := githubreconciler.NewClientCache(tokenSourceFunc)

	rec, err := branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			// key format: "owner/repo/module-path"
			// Example: "myorg/myapp/api/v1/users"
			parts := strings.Split(key, "/")
			if len(parts) < 3 {
				return "", "", "", fmt.Errorf("invalid key: %s", key)
			}
			owner, repo := parts[0], parts[1]
			module := strings.Join(parts[2:], "/")
			branch := "docs/" + strings.ReplaceAll(module, "/", "-")
			return owner, repo, branch, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			// Generate documentation from code comments
			// AI agent can improve docs based on validation feedback
			return fmt.Sprintf("Generate documentation (attempt %d)\n\nUpdated API docs", info.CommitCount+1), nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Validate documentation completeness and quality
			// - Check all public APIs are documented
			// - Run doc linter
			// - Verify examples compile
			docsValid := true // Placeholder for validation logic
			return docsValid, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Deploy documentation to hosting service
			fmt.Printf("Deploying docs for %s to production\n", info.Key)
			return nil
		}),
		branchreconciler.WithMaxAttempts(5),
		branchreconciler.WithRequeueDelay(2*time.Minute),
	)
	if err != nil {
		panic(err)
	}

	_ = rec // Reconciler is ready to be used
}

// ExampleReconciler_successCallbacks demonstrates different success callback patterns
// for various automation scenarios.
func ExampleReconciler_successCallbacks() {
	ctx := context.Background()

	var (
		tokenSourceFunc githubreconciler.TokenSourceFunc
		signer          git.Signer
	)

	cloneMeta := clonemanager.NewMeta(ctx, tokenSourceFunc, "automation-bot", signer)
	clientCache := githubreconciler.NewClientCache(tokenSourceFunc)

	// Example 1: Trigger CI/CD pipeline when quality gates pass
	_, _ = branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			return "myorg", "myapp", "auto/" + key, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			return "Automated changes", nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Check quality gates: tests pass, coverage threshold met, no security issues
			return true, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Trigger deployment pipeline
			fmt.Printf("Triggering deployment pipeline for %s at %s\n", info.Key, info.HeadSHA)
			return nil
		}),
	)

	// Example 2: Create pull request for human review
	_, _ = branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			return "myorg", "myapp", "refactor/" + key, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			return "Automated refactoring", nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Check that refactoring maintains test coverage
			return true, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Create PR for review
			fmt.Printf("Creating PR: %s/%s %s → %s\n",
				info.Owner, info.Repo, info.Branch, info.BaseBranch)
			return nil
		}),
	)

	// Example 3: Send notification when work completes
	_, _ = branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			return "myorg", "config-repo", "update/" + key, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			return "Update configuration", nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Validate configuration schema
			return true, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Notify team via Slack
			fmt.Printf("Configuration updated for %s - SHA: %s\n", info.Key, info.HeadSHA)
			return nil
		}),
	)

	// Example 4: Auto-merge when validation passes (no callback needed)
	_, _ = branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			return "myorg", "myapp", "codegen/" + key, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			return "Regenerate code", nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Check generated code compiles and tests pass
			return true, nil
		}),
		// No WithOnSuccess - just keep branch updated for external consumers
	)
}

// ExampleReconciler_configurationManagement demonstrates using branch reconciler
// for iterative configuration updates with validation.
func ExampleReconciler_configurationManagement() {
	ctx := context.Background()

	var (
		tokenSourceFunc githubreconciler.TokenSourceFunc
		signer          git.Signer
	)

	cloneMeta := clonemanager.NewMeta(ctx, tokenSourceFunc, "config-bot", signer)
	clientCache := githubreconciler.NewClientCache(tokenSourceFunc)

	rec, err := branchreconciler.New(
		cloneMeta,
		clientCache,
		branchreconciler.WithBranchNamer(func(key string) (string, string, string, error) {
			// key format: "environment/service"
			// Example: "production/api-gateway"
			parts := strings.Split(key, "/")
			if len(parts) != 2 {
				return "", "", "", fmt.Errorf("invalid key: %s", key)
			}
			branch := "config/" + parts[0] + "-" + parts[1]
			return "myorg", "infrastructure", branch, nil
		}),
		branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
			// Update configuration (e.g., Terraform, Kubernetes manifests)
			// Agent can adjust based on validation feedback
			return fmt.Sprintf("Update config (attempt %d)\n\nAdjusted for validation errors",
				info.CommitCount+1), nil
		}),
		branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
			// Validate configuration:
			// - Schema validation
			// - Policy checks
			// - Dry-run/plan succeeds
			configValid := true // Placeholder
			return configValid, nil
		}),
		branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
			// Apply configuration to target environment
			fmt.Printf("Applying configuration for %s\n", info.Key)
			return nil
		}),
		branchreconciler.WithMaxAttempts(3),
		branchreconciler.WithRequeueDelay(1*time.Minute),
	)
	if err != nil {
		panic(err)
	}

	_ = rec
}
