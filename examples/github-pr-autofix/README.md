# GitHub PR autofix

**Reconciler + agentic pattern** that extends the [PR validator](../github-pr-validator/)
with AI-powered auto-fixing using the metaagent framework.

**What it does:**
- Same validation as the PR validator, via the shared [`prvalidation`](../prvalidation/) library.
- When the `driftlessaf/autofix` label is present, the property `ENABLE_AUTOFIX`
  is set to `true` and validation fails, an AI agent running on Vertex AI
  automatically fixes the PR title and/or description.
- Supports both **Gemini** and **Claude** models, selected via the `AGENT_MODEL` env var,
  which defaults to `gemini-2.5-flash`.
- Updates the PR title to conventional commit format.
- Generates a meaningful description from PR context, including the list of validation
  issues and the list of changed files.
- Surfaces the model used, fixes applied, and agent reasoning in the Check Run output.
- Caps the number of agent invocations per PR generation to prevent loops,
  configurable via `MAX_FIX_ATTEMPTS` with a default of 2.

The reconciler concepts shared with the validator are documented in the
[main examples README](../README.md#key-concepts-validator-and-autofix); they cover the
StatusManager, the `Details` payload, the idempotent reconcile loop, workqueue
integration, and error handling.

## Configuration

| Env var | Default | Description |
|---------|---------|-------------|
| `ENABLE_AUTOFIX` | `false` | Master switch — when false, behaves identically to the validator |
| `AUTOFIX_LABEL` | `driftlessaf/autofix` | PR label that gates agent execution |
| `GCP_PROJECT_ID` | — | Vertex AI project. Required when `ENABLE_AUTOFIX=true`. |
| `GCP_REGION` | `us-central1` | Vertex AI region |
| `AGENT_MODEL` | `gemini-2.5-flash` | Model identifier passed to Vertex AI |
| `MAX_FIX_ATTEMPTS` | `2` | Max agent invocations per PR generation, where a generation is the hash of SHA, title, and body |

**Switching to Claude Sonnet:**

```bash
AGENT_MODEL=claude-sonnet-4-5@20250929
```

## Agent tools

The agent is given two tools, both backed by `func` callbacks against the GitHub
client so they can be unit-tested without a live API:

- `update_pr_title` — Updates the PR title. Validates the new title against
  `prvalidation.ConventionalCommitRegex` and a 72-character limit before calling GitHub.
- `update_pr_description` — Updates the PR body. Validates the new description is at
  least 20 characters after trimming.

After the agent finishes, the reconciler re-fetches the PR, re-runs `prvalidation.ValidatePR`,
and records the outcome in the Check Run via `statusmanager`. The recorded outcome includes
`FixesApplied`, `AgentReasoning`, and `ModelUsed`.

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
      │          │  ┌──────────────┐  ┌──────────────────────┐  ┌──────────────┐  │
      │          │  │   OctoSTS    │◄─│     Reconciler       │─►│  Metaagent   │  │
      │          │  │ (get token)  │  │ (validator + agent)  │  │  (Vertex AI) │  │
      │          │  └──────────────┘  └──────────┬───────────┘  └──────────────┘  │
      │          │                               │                                │
      └──────────┼───────────────────────────────┘                                │
  Check Run +    │    Create/Update Check Run + Update PR via GitHub API          │
  PR Updates     └────────────────────────────────────────────────────────────────┘
```

**Flow:**
1. GitHub sends a webhook when a PR is opened or edited.
2. `github-events` converts the webhook into a CloudEvent.
3. The CloudEvents Broker routes it onto the Workqueue.
4. The reconciler validates the PR title and description.
5. If validation fails, the `driftlessaf/autofix` label is present, and the fix-attempt
   budget is not exhausted:
   - Vertex AI is invoked through metaagent, using either Gemini or Claude as selected by `AGENT_MODEL`.
   - The agent calls `update_pr_title` and/or `update_pr_description`.
   - The reconciler re-validates the resulting PR state.
6. The Check Run is updated with the final results, including the model used and reasoning.

## File layout

```
github-pr-autofix/
└── cmd/reconciler/
    ├── main.go       # Reconciler with label gating, attempt budget, agent orchestration
    ├── agent.go      # Metaagent construction (Gemini/Claude via Vertex AI)
    ├── prtools.go    # PRTools callbacks + ToolProvider implementation
    ├── prompts.go    # System and user prompts
    ├── types.go      # PRContext (request) and PRFixResult (response)
    ├── doc.go        # Package docs
    └── main_test.go  # Unit tests
```

## Running tests

```bash
cd examples
go test ./github-pr-autofix/...
```

No GitHub credentials or Vertex AI access required — the agent path is exercised via
mocked `PRTools` callbacks.
