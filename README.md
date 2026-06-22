# DriftlessAF

[DriftlessAF](https://github.com/driftlessaf) is Chainguard's foundational
agentic framework for building AI-powered automation and resilient GitHub
reconcilers.

## Features

This project includes the following Go modules and functionality.

### Agentic AI infrastructure

- **AI executors**: Production-ready executors for Google Gemini and Anthropic Claude models.
- **Evaluation framework**: Testing and monitoring agent quality with comprehensive metrics.
- **OpenTelemetry metrics**: Built-in observability for AI operations.
- **Prompt building**: Utilities for constructing and managing prompts.
- **Tool calling**: Helpers for function/tool calling with Claude and Gemini.
- **Result parsing**: Structured output extraction from model responses.

Find more information in the [agents README](./agents/README.md).

### Reconciler infrastructure

Production-ready reconciler infrastructure based on the Kubernetes
reconciliation pattern, adapted for GitHub automation:

- **Workqueue system**: GCS-backed state persistence with retry, exponential
  backoff, and concurrency control (`workqueue/`). Find more information in the
  [workqueue README](./workqueue/README.md).
- **Reconcilers**: Process GitHub pull requests, repository file paths, APK
  packages, and OCI artifacts (`reconcilers/`). Find more information in the
  [reconciler README](./reconcilers/README.md).

## Installation

```bash
go get chainguard.dev/driftlessaf@latest
```

## Usage

See the package documentation for examples and the API reference.

## Examples

- [github-pr-validator](./examples/github-pr-validator/)
- [github-pr-autofix](./examples/github-pr-autofix/)
- [github-issue-materializer](./examples/github-issue-materializer/)
- [github-path-modernizer](./examples/github-path-modernizer/)

See the [examples README](./examples/README.md) for more information.

## Build

This repository contains two Go modules — the root `chainguard.dev/driftlessaf`
library and the `chainguard.dev/driftlessaf/examples` module. Build and test each
from its own directory.

Go 1.25.8 or newer is required, as specified in both `go.mod` files.

Root library:

```bash
go build ./...
go test ./...
```

Examples:

```bash
cd examples
go build ./...
go test ./...
```

## License

[Apache-2.0](./LICENSE)
