/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package branchreconciler provides a workqueue reconciler for iterative
// branch-based workflows without pull requests.
//
// Unlike the metareconciler which creates and manages PRs, branchreconciler
// works directly on branches for workflows where:
//   - An agent makes commits to fix builds or apply changes
//   - External criteria determine if the changes are acceptable
//   - Reconciliation continues until criteria is met or max attempts exceeded
//   - Upon success, a user-defined callback is executed (e.g., trigger workflow, create PR)
//
// # Usage
//
// Create a reconciler for automated dependency updates:
//
//	rec, err := branchreconciler.New(
//	    cloneMeta,
//	    clientCache,
//	    branchreconciler.WithBranchNamer(func(key string) (owner, repo, branch string, err error) {
//	        // Parse "owner/repo/dependency@version" → branch name
//	        parts := strings.Split(key, "/")
//	        owner, repo := parts[0], parts[1]
//	        depInfo := strings.Join(parts[2:], "/")
//	        return owner, repo, "deps/" + strings.ReplaceAll(depInfo, "@", "-"), nil
//	    }),
//	    branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *BranchInfo) (string, error) {
//	        // Update dependency and fix breaking changes
//	        return updateAgent.UpdateDependency(ctx, wt, info.Key)
//	    }),
//	    branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *BranchInfo) (bool, error) {
//	        // Check if all tests pass with the update
//	        return ciClient.CheckTestStatus(ctx, info.Owner, info.Repo, info.HeadSHA)
//	    }),
//	    branchreconciler.WithOnSuccess(func(ctx context.Context, info *BranchInfo) error {
//	        // Create PR for review or auto-merge if tests pass
//	        return ghClient.CreatePR(ctx, info.Owner, info.Repo, info.Branch, info.BaseBranch)
//	    }),
//	    branchreconciler.WithMaxAttempts(10),
//	    branchreconciler.WithRequeueDelay(5*time.Minute),
//	)
//
// # Reconciliation Flow
//
//  1. Parse workqueue key to determine GitHub owner/repo/branch
//  2. Check branch commit count (attempts) against max limit
//  3. Clone branch (or base branch if branch doesn't exist yet)
//  4. Execute agent function to make changes
//  5. Push commits to branch
//  6. Evaluate criteria function
//  7. If criteria not met: requeue for another attempt
//  8. If criteria met: execute success callback (if configured) and complete
//  9. If max attempts exceeded: fail permanently
//
// # Attempt Tracking
//
// Attempts are tracked via commit count on the branch (compared to base branch).
// Each reconciliation is expected to produce at least one commit, so commit count
// effectively equals attempt count.
//
// # Common Use Cases
//
//   - Dependency updates that iterate until tests pass
//   - Documentation generation that improves based on validation feedback
//   - Configuration updates that adjust based on policy checks
//   - Code generation that refines until linting passes
//   - Infrastructure changes that iterate until dry-run succeeds
//
// # When to Use This vs. metareconciler
//
// Use branchreconciler when:
//   - You want fully automated iteration without PR review
//   - Success criteria is external (CI status, API calls, metrics)
//   - You need commit history showing iteration attempts
//   - Success should trigger automated actions (deploy, notify)
//
// Use metareconciler when:
//   - You need human review via pull requests
//   - You're working from GitHub issues
//   - Success is determined by GitHub CI checks
//   - Changes require approval before merging
package branchreconciler
