# prvalidation — shared PR validation library

Shared validation logic used by the [`github-pr-validator`](../github-pr-validator/) and
[`github-pr-autofix`](../github-pr-autofix/) examples. It validates a PR title against the
[conventional commit](https://www.conventionalcommits.org/) format, checks that a description
meets a minimum length, and renders a markdown report for GitHub Check Runs.

## What it provides

- `ValidatePR(title, body)` — returns `(titleValid, descValid, issues)`.
- `ConventionalCommitRegex` — exported regex for callers that need to validate titles outside
  the full `ValidatePR` flow. The autofix agent's `update_pr_title` tool uses it.
- `ConventionalPrefixes` — the list of accepted types: `feat`, `fix`, `docs`, `style`,
  `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- `Details` — `statusmanager` payload struct with a `Markdown()` method for the Check Run body.
- `ComputeGeneration(sha, title, body)` — SHA-256 hash used as an idempotency key so a Check Run
  is re-evaluated when the PR title or body changes, not only when a new commit is pushed.

## Validation rules

**Title** — must match `<type>: <description>` or `<type>(<scope>): <description>` with a
space after the colon. Valid title examples:

```
feat: add new feature
fix(auth): resolve login bug
docs: update README
refactor(api): simplify handlers
```

**Description** — trimmed body must be non-empty and at least 20 characters. Anything shorter
yields an issue describing the shortfall.

## `Details` struct

`Details` is the payload persisted with each Check Run via `statusmanager`. It carries the
validation outcome plus optional fields the autofix example populates when the agent runs:

| Field | Set by | Purpose |
|-------|--------|---------|
| `Generation` | both | Idempotency key from `ComputeGeneration` |
| `TitleValid`, `DescriptionValid` | both | Per-check outcome |
| `Issues` | both | Human-readable issue list rendered in the Check Run |
| `AgentEnabled`, `FixesApplied`, `AgentReasoning`, `FixAttempts`, `ModelUsed` | autofix only | Agent activity surfaced in `Markdown()` |

`Markdown()` renders a table of check results, an Issues section when present, and an Agent
Activity section when `AgentEnabled` is true.

## File layout

```
prvalidation/
├── doc.go              # Package docs
├── validation.go       # ValidatePR, regex, Details, Markdown, ComputeGeneration
├── validation_test.go  # Unit tests for validation rules
└── example_test.go     # Runnable examples
```

## Running tests

```bash
cd examples
go test ./prvalidation/...
```

No GitHub credentials or external services required.
