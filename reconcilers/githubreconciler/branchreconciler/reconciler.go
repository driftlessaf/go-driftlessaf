/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package branchreconciler

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/go-git/go-git/v5"
	"github.com/google/go-github/v84/github"
)

// Reconciler orchestrates branch-based reconciliation with agent iteration.
type Reconciler struct {
	workqueue.UnimplementedWorkqueueServiceServer

	cloneMeta   *clonemanager.Meta
	clientCache *githubreconciler.ClientCache

	branchNamer  BranchNamer
	agentFunc    AgentFunc
	criteriaFunc CriteriaFunc
	onSuccess    SuccessFunc

	baseBranch   string        // Default: "main"
	maxAttempts  int           // Max reconciliation attempts
	requeueDelay time.Duration // Delay between attempts
}

// BranchNamer converts workqueue key to GitHub coordinates.
// Example: "melange/3.11.1" → owner="wolfi-dev", repo="os", branch="fix-bot/melange-3.11.1"
type BranchNamer func(key string) (owner, repo, branch string, err error)

// AgentFunc executes on the git worktree to make changes.
// Returns commit message for the changes made.
type AgentFunc func(ctx context.Context, wt *git.Worktree, info *BranchInfo) (commitMessage string, err error)

// CriteriaFunc evaluates if the branch changes meet acceptance criteria.
type CriteriaFunc func(ctx context.Context, info *BranchInfo) (met bool, err error)

// SuccessFunc is called when criteria is met after successful reconciliation.
// This could trigger workflows, create PRs, send notifications, etc.
// If nil, reconciliation completes without additional actions.
type SuccessFunc func(ctx context.Context, info *BranchInfo) error

// BranchInfo provides context about the current branch state.
type BranchInfo struct {
	Key         string // Original workqueue key
	Owner       string // GitHub owner
	Repo        string // GitHub repo
	Branch      string // Branch name
	BaseBranch  string // Base branch (e.g., "main")
	HeadSHA     string // Current HEAD SHA
	CommitCount int    // Number of commits on branch vs base
	BaseCommit  string // Base commit SHA (for comparison)
}

// Reconcile performs the branch-based reconciliation for a given key.
func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// 1. Parse key to determine branch
	owner, repo, branch, err := r.branchNamer(key)
	if err != nil {
		return workqueue.NonRetriableError(err, "invalid key format")
	}

	// 2. Get GitHub client
	client, err := r.clientCache.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get github client: %w", err)
	}

	// 3. Check if branch exists and get commit count
	commitCount, baseCommit, headSHA, err := getBranchInfo(ctx, client, owner, repo, branch, r.baseBranch)
	if err != nil {
		return fmt.Errorf("get branch info: %w", err)
	}

	// 4. Check attempt limit
	if commitCount >= r.maxAttempts {
		return workqueue.NonRetriableError(
			fmt.Errorf("max attempts (%d) exceeded", r.maxAttempts),
			"branch has reached maximum reconciliation attempts",
		)
	}

	// 5. Clone branch (or base if branch doesn't exist)
	cloneMgr, err := r.cloneMeta.Get(owner, repo)
	if err != nil {
		return fmt.Errorf("get clone manager: %w", err)
	}

	var lease *clonemanager.Lease
	resource := &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: owner,
		Repo:  repo,
		Path:  "/", // Dummy path for leasing
	}

	if headSHA != "" {
		// Branch exists - lease it
		lease, err = cloneMgr.LeaseRef(ctx, resource, branch)
	} else {
		// Branch doesn't exist - lease base and will create branch
		lease, err = cloneMgr.LeaseRef(ctx, resource, r.baseBranch)
		if err == nil {
			headSHA = lease.SHA() // Will be base SHA
		}
	}
	if err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}
	defer func() {
		if err := lease.Return(ctx); err != nil {
			clog.WarnContextf(ctx, "Failed to return lease: %v", err)
		}
	}()

	// 6. Build BranchInfo for callbacks
	info := &BranchInfo{
		Key:         key,
		Owner:       owner,
		Repo:        repo,
		Branch:      branch,
		BaseBranch:  r.baseBranch,
		HeadSHA:     headSHA,
		CommitCount: commitCount,
		BaseCommit:  baseCommit,
	}

	// 7. Run agent and push changes
	err = lease.MakeAndPushChanges(ctx, branch, func(ctx context.Context, wt *git.Worktree) (string, error) {
		return r.agentFunc(ctx, wt, info)
	})
	if err != nil {
		return fmt.Errorf("agent execution failed: %w", err)
	}

	// 8. Update BranchInfo with new SHA after push
	newHeadSHA, err := getBranchHeadSHA(ctx, client, owner, repo, branch)
	if err != nil {
		return fmt.Errorf("get new head sha: %w", err)
	}
	info.HeadSHA = newHeadSHA
	info.CommitCount = commitCount + 1 // Incremented by agent's commit

	// 9. Evaluate criteria
	met, err := r.criteriaFunc(ctx, info)
	if err != nil {
		return fmt.Errorf("criteria evaluation failed: %w", err)
	}

	// 10. If criteria not met, requeue for another attempt
	if !met {
		clog.InfoContextf(ctx, "Criteria not met, requeuing for attempt %d/%d",
			info.CommitCount+1, r.maxAttempts)
		return workqueue.RequeueAfter(r.requeueDelay)
	}

	// 11. Criteria met - execute success callback if configured
	if r.onSuccess != nil {
		if err := r.onSuccess(ctx, info); err != nil {
			return fmt.Errorf("success callback failed: %w", err)
		}
		clog.InfoContextf(ctx, "Reconciliation complete, success callback executed for %s", key)
	} else {
		clog.InfoContextf(ctx, "Reconciliation complete for %s (no success callback configured)", key)
	}

	return nil
}

// getBranchInfo queries GitHub to get branch state.
func getBranchInfo(ctx context.Context, client *github.Client, owner, repo, branch, base string) (commitCount int, baseCommit, headSHA string, err error) {
	// Try to get branch
	branchRef, resp, err := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		// Branch doesn't exist yet
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// Get base branch commit
			baseRef, _, err := client.Repositories.GetBranch(ctx, owner, repo, base, 0)
			if err != nil {
				return 0, "", "", fmt.Errorf("get base branch: %w", err)
			}
			return 0, baseRef.GetCommit().GetSHA(), "", nil
		}
		return 0, "", "", err
	}

	headSHA = branchRef.GetCommit().GetSHA()

	// Compare branch to base to get commit count
	comparison, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, branch, nil)
	if err != nil {
		return 0, "", "", fmt.Errorf("compare commits: %w", err)
	}

	commitCount = comparison.GetAheadBy()
	baseCommit = comparison.GetMergeBaseCommit().GetSHA()

	return commitCount, baseCommit, headSHA, nil
}

// getBranchHeadSHA gets the current HEAD SHA of a branch.
func getBranchHeadSHA(ctx context.Context, client *github.Client, owner, repo, branch string) (string, error) {
	branchRef, _, err := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return "", err
	}
	return branchRef.GetCommit().GetSHA(), nil
}

// Process implements workqueue.WorkqueueServiceServer.
func (r *Reconciler) Process(ctx context.Context, req *workqueue.ProcessRequest) (*workqueue.ProcessResponse, error) {
	clog.InfoContextf(ctx, "Processing branch reconciliation: %s (priority: %d)", req.Key, req.Priority)

	err := r.Reconcile(ctx, req.Key)
	if err != nil {
		// Check for requeue request
		if delay, floor, ok := workqueue.GetRequeueOptions(err); ok {
			clog.InfoContextf(ctx, "Reconciliation requested requeue after %v (floor=%t) for key: %s", delay, floor, req.Key)
			return &workqueue.ProcessResponse{RequeueAfterSeconds: int64(delay.Seconds()), RequeueFloor: floor}, nil
		}

		// Check for non-retriable error
		if details := workqueue.GetNonRetriableDetails(err); details != nil {
			clog.WarnContextf(ctx, "Reconciliation failed with non-retriable error for key %s: %v (reason: %s)",
				req.Key, err, details.Message)
			return &workqueue.ProcessResponse{}, nil
		}

		// Regular error - will retry with backoff
		clog.ErrorContextf(ctx, "Reconciliation failed for key %s: %v", req.Key, err)
		return nil, err
	}

	clog.InfoContextf(ctx, "Successfully reconciled branch: %s", req.Key)
	return &workqueue.ProcessResponse{}, nil
}
