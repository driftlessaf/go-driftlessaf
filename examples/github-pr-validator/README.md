# GitHub PR validator

**Reconciler pattern** that validates GitHub pull requests and creates Check Runs with the validation results.

**What it does:**
- Validates PR title follows [conventional commit](https://www.conventionalcommits.org/) format
- Validates PR description is not empty or too short
- Creates a GitHub Check Run showing pass/fail status
- Uses `statusmanager` for idempotent check run management

Validation rules and the `Details` payload come from the shared
[`prvalidation`](../prvalidation/) library. The reconciler concepts used here are
documented in the [main examples README](../README.md#key-concepts-validator-and-autofix);
they cover the StatusManager, the `Details` payload, the idempotent reconcile loop,
workqueue integration, and error handling.

## Architecture

```
 ┌──────────┐                              GCP
 │  GitHub  │    ┌────────────────────────────────────────────────────────────────┐
 │          │    │                                                                │
 │  PR open │    │  ┌─────────────┐   ┌──────────────────────┐                    │
 │  PR edit ├────┼─►│github-events├──►│  CloudEvents Broker  │                    │
 │          │    │  │  (webhook)  │   │  (filter + enqueue)  │                    │
 │          │    │  └─────────────┘   └──────────┬───────────┘                    │
 └────▲─────┘    │                               │                                │
      │          │                               ▼                                │
      │          │                    ┌──────────────────────┐                    │
      │          │                    │     Workqueue        │                    │
      │          │                    │  (rcv + dispatcher)  │                    │
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
1. GitHub sends a webhook when a PR is opened or edited.
2. `github-events` converts the webhook into a CloudEvent.
3. The CloudEvents Broker routes it onto the Workqueue.
4. The reconciler validates the PR title and description via `prvalidation.ValidatePR`.
5. It creates or updates the Check Run with pass/fail status using `statusmanager`.

## File layout

```
github-pr-validator/
└── cmd/reconciler/
    ├── main.go       # Reconciler entry point and PR validation orchestration
    └── main_test.go  # Unit tests
```

## Running tests

```bash
cd examples
go test ./github-pr-validator/...
```

No GitHub credentials or external services required.
