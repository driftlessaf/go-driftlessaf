# branchreconciler

Branch-based reconciler for iterative, criteria-driven workflows without pull requests.

## Overview

The `branchreconciler` package provides a workqueue reconciler designed for fully automated, iterative workflows where:

- An agent makes commits to apply changes directly to a branch
- User-defined criteria determine if the changes are acceptable
- Reconciliation continues until criteria is met or max attempts exceeded
- Upon success, a customizable callback is executed (deploy, notify, create PR, etc.)
- No PRs are created during iteration - all work happens on the branch itself

This is different from `metareconciler` which creates and manages pull requests for human review workflows.

## Use Cases

### Automated Dependency Updates
1. Agent updates dependency and fixes breaking changes (commits to branch)
2. Check if all tests pass with the update
3. If tests fail → agent adjusts fix (more commits)
4. If tests pass → create PR for review or auto-merge
5. Iteration continues until tests pass or max attempts exceeded

### Configuration Management
1. Agent generates/updates configuration (Terraform, K8s manifests)
2. Validate schema, run policy checks, execute dry-run
3. If validation fails → agent adjusts configuration
4. If validation passes → apply to target environment
5. Iteration continues until validation passes

### Documentation Generation
1. Agent generates documentation from code
2. Validate completeness, run linters, check examples compile
3. If validation fails → agent improves documentation
4. If validation passes → deploy to hosting service
5. Iteration continues until quality gates met

### Code Generation
1. Agent generates code from schema/templates
2. Check if code compiles and passes linting
3. If checks fail → agent refines generated code
4. If checks pass → update branch for external consumers
5. Iteration continues until code is valid

## Architecture

### Core Components

- **Reconciler**: Orchestrates the branch-based reconciliation loop
- **BranchNamer**: Converts workqueue keys to GitHub coordinates (owner/repo/branch)
- **AgentFunc**: Executes on the git worktree to make changes
- **CriteriaFunc**: Evaluates if branch changes meet acceptance criteria (user-defined)
- **SuccessFunc**: Executed when criteria is met (trigger workflow, create PR, notify, etc.)

### Attempt Tracking

Attempts are tracked via commit count on the branch (compared to base branch). Each reconciliation produces at least one commit, so commit count effectively equals attempt count.

## Usage

```go
import (
    "chainguard.dev/driftlessaf/reconcilers/githubreconciler/branchreconciler"
    "chainguard.dev/driftlessaf/reconcilers/githubreconciler"
    "chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
)

// Example: Automated dependency updates
rec, err := branchreconciler.New(
    cloneMeta,
    clientCache,
    branchreconciler.WithBranchNamer(func(key string) (owner, repo, branch string, err error) {
        // Parse "owner/repo/dependency@version" → branch name
        parts := strings.Split(key, "/")
        owner, repo := parts[0], parts[1]
        depInfo := strings.Join(parts[2:], "/")
        return owner, repo, "deps/" + strings.ReplaceAll(depInfo, "@", "-"), nil
    }),
    branchreconciler.WithAgentFunc(func(ctx context.Context, wt *git.Worktree, info *branchreconciler.BranchInfo) (string, error) {
        // Update dependency and fix breaking changes
        return updateAgent.UpdateDependency(ctx, wt, info.Key)
    }),
    branchreconciler.WithCriteriaFunc(func(ctx context.Context, info *branchreconciler.BranchInfo) (bool, error) {
        // Check if all tests pass with the update
        return ciClient.CheckTestStatus(ctx, info.Owner, info.Repo, info.HeadSHA)
    }),
    branchreconciler.WithOnSuccess(func(ctx context.Context, info *branchreconciler.BranchInfo) error {
        // Create PR for review when tests pass
        return ghClient.CreatePR(ctx, info.Owner, info.Repo, info.Branch, info.BaseBranch)
    }),
    branchreconciler.WithMaxAttempts(10),
    branchreconciler.WithRequeueDelay(5*time.Minute),
)

// Use with workqueue dispatcher
dispatcher.Handle(ctx, wq, concurrency, batchSize, dispatcher.ServiceCallback(rec))
```

## Reconciliation Flow

1. Parse workqueue key to determine GitHub owner/repo/branch
2. Check branch commit count (attempts) against max limit
3. Clone branch (or base branch if branch doesn't exist yet)
4. Execute agent function to make changes
5. Push commits to branch
6. Evaluate criteria function (user-defined success criteria)
7. If criteria not met: requeue for another attempt
8. If criteria met: execute success callback and complete
9. If max attempts exceeded: fail permanently

## Configuration Options

- **WithBranchNamer**: Required. Converts workqueue key to GitHub coordinates
- **WithAgentFunc**: Required. Agent function that makes changes
- **WithCriteriaFunc**: Required. Evaluates if changes meet success criteria
- **WithOnSuccess**: Optional. Callback executed when criteria is met (deploy, create PR, notify, etc.)
- **WithBaseBranch**: Default "main". Base branch to compare against
- **WithMaxAttempts**: Default 10. Maximum reconciliation attempts
- **WithRequeueDelay**: Default 5 minutes. Delay between attempts

## BranchInfo

The `BranchInfo` struct provides context to callbacks:

```go
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
```

## Integration

The reconciler integrates with:

- **clonemanager**: For git clone management and worktree operations
- **githubreconciler.ClientCache**: For managing GitHub API clients
- **workqueue**: For dispatcher integration and error handling

## Error Handling

- **workqueue.RequeueAfter**: Returned when criteria not met, triggers requeue with delay
- **workqueue.NonRetriableError**: Returned when max attempts exceeded or invalid key format
- Regular errors trigger retry with exponential backoff

## When to Use This vs. metareconciler

**Use branchreconciler when:**
- You want fully automated iteration without PR review
- Success criteria is external (CI status, API calls, metrics, validation)
- You need commit history showing iteration attempts
- Success should trigger automated actions (deploy, notify, create PR)
- No human approval required in the loop

**Use metareconciler when:**
- You need human review via pull requests
- You're working from GitHub issues
- Success is determined by GitHub CI checks
- Changes require approval before merging
- PR-based workflow with CI feedback loop

## Examples

See [example_test.go](./example_test.go) for complete examples including:
- Automated dependency updates
- Documentation generation
- Configuration management
- CI/CD pipeline integration
- Multiple success callback patterns

## See Also

- [example_test.go](./example_test.go) - Comprehensive usage examples
- [reconciler_test.go](./reconciler_test.go) - Unit tests
- [doc.go](./doc.go) - Package documentation
- [metareconciler](../metareconciler/) - PR-based reconciler alternative
