# GitHub Issue Materializer

This example demonstrates how to build a GitHub issue materializer that transforms
problem statements in GitHub issues into pull requests using AI-generated code.

## Features

- **Provider-agnostic AI execution** using metaagent (supports Gemini and Claude)
- **GitHub issue-based reconciliation** using metareconciler
- **Composable tools** using the toolcall provider pattern
- **Automatic PR creation** from AI-generated code changes

## Architecture

The materializer operates in two modes:

### FRESH mode

When processing a new issue, the agent:

1. Analyzes the problem statement from the issue body
2. Explores the codebase using `list_directory` and `search_codebase` tools
3. Reads relevant files to understand patterns and conventions
4. Implements the solution using `write_file` and `delete_file` tools
5. Returns a summary and conventional commit message

### ITERATION mode

When a PR exists with CI failures (findings):

1. The agent examines findings from the previous attempt
2. Uses `get_finding_details` to understand what went wrong
3. Makes targeted fixes to address the specific failures
4. Preserves working parts of the previous implementation

## Tool Composition

The materializer uses composed metaagent tools:

- **WorktreeTools**: `read_file`, `write_file`, `delete_file`, `list_directory`, `search_codebase`
- **FindingTools**: `get_finding_details` (for iteration on CI failures)

Tools are composed using the provider pattern:

```go
type materializerTools = toolcall.FindingTools[toolcall.WorktreeTools[toolcall.EmptyTools]]

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
| `MATERIALIZER_MODEL` | AI model to use | `gemini-2.5-flash` |
| `MATERIALIZER_REGION` | GCP region for the model | `us-central1` |

## Integration

The reconciler integrates with the GitHub webhook system:

1. GitHub sends issue events to the workqueue
2. The reconciler receives issue URLs and fetches the issue
3. Issues with the `{identity}/managed` label are processed
4. The agent implements the solution and creates/updates a PR
5. CI runs and findings are recorded for iteration

## Usage

See `cmd/reconciler/` for the complete implementation:

- `main.go` - Server setup and configuration
- `agent.go` - Agent configuration with prompts and response types
- `reconciler.go` - Reconciler construction using metareconciler
