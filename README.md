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

See the package documentation for examples and API reference.

## License

[Apache-2.0](./LICENSE)
