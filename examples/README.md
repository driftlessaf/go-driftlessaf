# DriftlessAF Examples

Minimal hello-world reconcilers demonstrating the DriftlessAF workqueue pattern for GitHub automation.

## GitHub PR Validator (`github-pr-hello/`)

A reconciler that validates GitHub pull requests and creates Check Runs with the validation results.

**What it does:**
- Validates PR title follows [conventional commit](https://www.conventionalcommits.org/) format
- Validates PR description is not empty or too short
- Creates a GitHub Check Run showing pass/fail status
- Uses `statusmanager` for idempotent check run management

**Valid title formats:**
```
feat: add new feature
fix(auth): resolve login bug
docs: update README
refactor(api): simplify handlers
```

## Architecture

```
 ┌──────────┐                              GCP
 │  GitHub  │    ┌────────────────────────────────────────────────────────────────┐
 │          │    │                                                                │
 │  PR open │    │  ┌─────────────┐   ┌──────────────────────┐                    │
 │  PR edit ├────┼─►│github-events├──►│  CloudEvents Bridge  │                    │
 │          │    │  │  (webhook)  │   │  (filter + enqueue)  │                    │
 │          │    │  └─────────────┘   └──────────┬───────────┘                    │
 └────▲─────┘    │                               │                                │
      │          │                               ▼                                │
      │          │                    ┌──────────────────────┐                    │
      │          │                    │  Workqueue Receiver  │                    │
      │          │                    │      (-wq-rcv)       │                    │
      │          │                    └──────────┬───────────┘                    │
      │          │                               │                                │
      │          │                               ▼                                │
      │          │                    ┌──────────────────────┐                    │
      │          │                    │ Workqueue Dispatcher │                    │
      │          │                    │      (-wq-dsp)       │                    │
      │          │                    └──────────┬───────────┘                    │
      │          │                               │                                │
      │          │                               ▼                                │
      │          │  ┌──────────────┐  ┌──────────────────────┐                    │
      │          │  │   OctoSTS    │◄─│     Reconciler       │                    │
      │          │  │ (get token)  │  │    (validator)       │                    │
      │          │  └──────────────┘  └──────────┬───────────┘                    │
      │          │                               │                                │
      └──────────┼───────────────────────────────┘                                │
    Check Run    │         Create/Update Check Run via GitHub API                 │
                 └────────────────────────────────────────────────────────────────┘
```

**Flow:**
1. GitHub sends webhook when PR is opened/edited
2. `github-events` converts webhook to CloudEvent
3. CloudEvents Bridge filters for `dev.chainguard.github.pull_request` events
4. Workqueue Receiver accepts and deduplicates work items (by PR URL)
5. Workqueue Dispatcher dispatches work to Reconciler (with concurrency control)
6. Reconciler requests GitHub token from OctoSTS
7. Reconciler fetches PR details, validates title/description
8. Reconciler creates/updates Check Run with validation results

## Project Structure

```
github-pr-hello/
├── cmd/reconciler/
│   ├── main.go       # Reconciler service implementation
│   └── main_test.go  # Unit tests
└── go.mod
```

## Running Tests

The tests don't require GitHub credentials or any external services.

```bash
cd driftlessaf/examples
go test -v ./...
```

## Key Concepts

### StatusManager

The reconciler uses `statusmanager` to manage GitHub Check Runs. This provides:
- **Idempotency**: Checks `ObservedState()` before processing to skip already-processed states
- **State persistence**: Stores validation details in the check run for future reference
- **Cloud Logging URL**: Automatically links check runs to Cloud Logging for debugging

```go
// Create status manager at startup
sm, err := statusmanager.NewStatusManager[Details](ctx, cfg.OctoIdentity)

// In reconciler: create a session for this SHA
session := sm.NewSession(gh, res, sha)

// Compute generation key from SHA + title + body
// This ensures re-validation when PR metadata changes, not just code
generation := computeGeneration(sha, pr.GetTitle(), pr.GetBody())

// Check if already processed (idempotency)
// IMPORTANT: Store generation in Details, not ObservedGeneration
// (statusmanager always sets ObservedGeneration to SHA)
observed, err := session.ObservedState(ctx)
if observed != nil && observed.Status == "completed" && observed.Details.Generation == generation {
    return nil // Skip - already processed this exact state
}

// Update status with validation results
status := &statusmanager.Status[Details]{
    Status:     "completed",
    Conclusion: "success", // or "failure"
    Details:    Details{
        Generation:       generation,  // Store for idempotency
        TitleValid:       true,
        DescriptionValid: true,
    },
}
return session.SetActualState(ctx, "All checks passed!", status)
```

> **Note**: The `ObservedGeneration` field in `statusmanager.Status` is always set to the
> commit SHA by the statusmanager. For custom idempotency keys (like title+body hash),
> store them in your `Details` struct. This pattern is used by production bots like qackage.

### Details Struct

Define a `Details` struct to hold reconciler-specific state. Implement `Markdown()` to render the check run output:

```go
type Details struct {
    // Generation stores a custom idempotency key (e.g., hash of SHA + title + body)
    // This allows re-validation when PR metadata changes, not just code
    Generation       string   `json:"generation"`
    TitleValid       bool     `json:"titleValid"`
    DescriptionValid bool     `json:"descriptionValid"`
    Issues           []string `json:"issues,omitempty"`
}

func (d Details) Markdown() string {
    // Return markdown for check run output
}

// computeGeneration creates a unique key from SHA, title, and body
func computeGeneration(sha, title, body string) string {
    h := sha256.New()
    h.Write([]byte(sha))
    h.Write([]byte(title))
    h.Write([]byte(body))
    return hex.EncodeToString(h.Sum(nil))
}
```

### Reconciler Pattern

The reconciler receives work items from a workqueue and processes them idempotently:

```go
func reconcilePR(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, sm *statusmanager.StatusManager[Details]) error {
    // 1. Fetch current state
    pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, res.Number)

    // 2. Skip if not applicable
    if pr.GetState() == "closed" {
        return nil
    }

    // 3. Create session and check observed state
    session := sm.NewSession(gh, res, pr.GetHead().GetSHA())

    // 4. Validate and compute desired state
    titleValid, descValid, issues := validatePR(pr.GetTitle(), pr.GetBody())

    // 5. Update status via statusmanager
    return session.SetActualState(ctx, summary, status)
}
```

### Workqueue Integration

The reconciler implements `WorkqueueServiceServer.Process()`:

```go
workqueue.RegisterWorkqueueServiceServer(server, githubreconciler.NewReconciler(
    clientCache,
    githubreconciler.WithReconciler(newReconciler(sm)),
))
```

### Error Handling

- **Retriable errors**: Return standard error → workqueue retries with backoff
- **Non-retriable errors**: Use `workqueue.NonRetriableError()` → skip retries
- **Delayed requeue**: Use `workqueue.RequeueAfter(duration)` → retry after delay

## Deployment

Deploy reconcilers to GCP using the Terraform modules from [driftlessaf/terraform-infra-reconcilers](https://github.com/driftlessaf/terraform-infra-reconcilers).
