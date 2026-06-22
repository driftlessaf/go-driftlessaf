# DriftlessAF examples

Examples demonstrating the DriftlessAF reconciler pattern for GitHub automation.

## Examples

- **[github-pr-validator/](./github-pr-validator/)** — Reconciler that checks PR titles
  for conformance with the [conventional commits](https://www.conventionalcommits.org/)
  specification and validates descriptions, reporting results via GitHub Check Runs.
- **[github-pr-autofix/](./github-pr-autofix/)** — The validator extended with an AI agent
  running on Vertex AI, using either Gemini or Claude models, that fixes the PR title and
  description when the `driftlessaf/autofix` label is present.
- **[github-issue-materializer/](./github-issue-materializer/)** — Issue-based agent, built
  on `metareconciler`, that turns problem statements in GitHub issues into PRs using
  AI-generated code.
- **[github-path-modernizer/](./github-path-modernizer/)** — Path-based agent, built on
  `metapathreconciler`, that runs the Go modernize analysis suite and produces PRs with
  AI-applied fixes.

Shared library:

- **[prvalidation/](./prvalidation/)** — Conventional-commit and description validation,
  the `Details` payload, and `ComputeGeneration` idempotency key used by both PR examples.

## Running tests

The tests don't require GitHub credentials or any external services.

```bash
cd examples
go test -v ./...
```

## Key concepts (validator and autofix)

The patterns below apply to the `github-pr-validator` and `github-pr-autofix` examples,
which build directly on `githubreconciler` + `statusmanager`. The `github-issue-materializer`
and `github-path-modernizer` examples use the higher-level `metareconciler` and
`metapathreconciler` abstractions respectively, and have their own architectural docs in
their READMEs.

### StatusManager

The reconciler uses `statusmanager` to manage GitHub Check Runs. This provides:

- **Idempotency**: checks `ObservedState()` before processing to skip already-processed states.
- **State persistence**: stores validation details in the check run for future reference.
- **Cloud Logging URL**: automatically links check runs to Cloud Logging for debugging.

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
> commit SHA by the statusmanager. For custom idempotency keys, such as a hash of the
> title and body, store them in your `Details` struct. This pattern is used by production
> bots like qackage.

### Details struct

Define a `Details` struct to hold reconciler-specific state. Implement `Markdown()` to render
the check run output. The shared [`prvalidation`](./prvalidation/) package defines the
`Details` type used by both PR examples:

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

### Reconciler pattern

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

### Workqueue integration

The reconciler implements `WorkqueueServiceServer.Process()`:

```go
workqueue.RegisterWorkqueueServiceServer(server, githubreconciler.NewReconciler(
    clientCache,
    githubreconciler.WithReconciler(newReconciler(sm)),
))
```

### Error handling

- **Retriable errors**: return a standard error → workqueue retries with backoff.
- **Non-retriable errors**: use `workqueue.NonRetriableError()` → skip retries.
- **Delayed requeue**: use `workqueue.RequeueAfter(duration)` → retry after delay.

## Deployment

Deploy reconcilers to GCP using the Terraform modules from
[driftlessaf/terraform-infra-reconcilers](https://github.com/driftlessaf/terraform-infra-reconcilers).
