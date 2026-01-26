# DriftlessAF

DriftlessAF is Chainguard's foundational framework for building AI-powered automation and resilient GitHub reconcilers.

## Features

### AI Agent Infrastructure

- **AI Executors**: Production-ready executors for Google Gemini and Anthropic Claude models (`agents/executor/`)
- **Evaluation Framework**: Testing and monitoring agent quality with comprehensive metrics (`agents/evals/`)
- **OpenTelemetry Metrics**: Built-in observability for AI operations (`agents/metrics/`)
- **Prompt Building**: Utilities for constructing and managing prompts (`agents/promptbuilder/`)
- **Tool Calling**: Helpers for function/tool calling with Claude and Gemini (`agents/toolcall/`)
- **Result Parsing**: Structured output extraction from model responses (`agents/result/`)

### Reconciler Infrastructure

Production-ready reconciler infrastructure based on the Kubernetes reconciliation pattern, adapted for GitHub automation:

- **Workqueue System**: GCS-backed state persistence with retry, exponential backoff, and concurrency control (`workqueue/`)
- **GitHub Reconcilers**: Process GitHub pull requests, file paths, APK packages, and OCI artifacts (`reconcilers/`, `githubreconciler/`)

## Installation

```bash
go get chainguard.dev/driftlessaf@latest
```

## Usage

See the package documentation for examples and API reference.

## License

Apache-2.0
