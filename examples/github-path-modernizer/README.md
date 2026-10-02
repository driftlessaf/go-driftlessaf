# GitHub Path Modernizer

This example demonstrates how to build a path-based GitHub reconciler that
applies Go modernize fixes using AI-generated code changes.

## Features

- **Provider-agnostic AI execution** using metaagent (supports Gemini and Claude)
- **Path-based reconciliation** using metapathreconciler
- **Static analysis** using the Go modernize analysis suite
- **PR check annotations** for diagnostics found on existing PRs
- **Composable tools** using the toolcall provider pattern
- **Automatic PR creation** from AI-generated modernize fixes

## Architecture

The modernizer operates in two modes depending on the resource type:

### Path reconciliation

When processing a file path URL:

1. Runs the Go modernize analyzer on the path to discover diagnostics
2. If no diagnostics are found, closes any stale PR
3. Converts diagnostics to findings and passes them to the AI agent
4. The agent reads the affected files and applies the modernize fixes
5. Creates or updates a PR with the changes

On subsequent reconciliations (CI failures):

1. Feeds CI check findings to the agent instead of re-running the analyzer
2. The agent fixes the specific CI failures while preserving working changes

### PR reconciliation

When processing a pull request URL:

1. If the PR branch matches our identity prefix, re-queues the original path
2. Otherwise, runs the analyzer on changed files and reports diagnostics as
   GitHub check annotations (filtered to only lines touched in the diff)

## Tool Composition

The modernizer uses composed metaagent tools:

- **WorktreeTools**: `read_file`, `write_file`, `delete_file`, `list_directory`, `search_codebase`
- **FindingTools**: `get_finding_details` (for iteration on CI failures)

Tools are composed using the provider pattern:

```go
type modernizerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

tools := toolcall.NewFindingToolsProvider[*Result, toolcall.WorktreeTools[toolcall.EmptyTools]](
    toolcall.NewWorktreeToolsProvider[*Result, toolcall.EmptyTools](
        toolcall.NewEmptyToolsProvider[*Result](),
    ),
)
```

## Configuration

The service is configured via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `PORT` | Server port | `8080` |
| `OCTO_IDENTITY` | OctoSTS identity for GitHub authentication | (required) |
| `METRICS_PORT` | Prometheus metrics port | `2112` |
| `ENABLE_PPROF` | Enable pprof endpoints | `false` |
| `MODEL` | AI model to use | `gemini-3.8-flash` |
| `MODEL_REGION` | GCP region for the model | `global` |

## Integration

The reconciler integrates with the GitHub webhook system:

1. File path URLs are sent to the workqueue (e.g., from a periodic scanner)
2. The reconciler runs the Go modernize analyzer on each path
3. Diagnostics are fed to the AI agent, which applies fixes
4. A PR is created/updated with the modernize changes
5. PR events trigger check annotations for diagnostics on changed lines
6. CI failures are fed back to the agent for iteration

## Usage

See `cmd/reconciler/` for the complete implementation:

- `main.go` - Server setup, tool composition, and agent/clone infrastructure
- `agent.go` - Agent configuration with prompts and response types
- `reconciler.go` - Reconciler construction using metapathreconciler
- `analyzer.go` - Go modernize analyzer implementation
