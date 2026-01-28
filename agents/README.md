# DriftlessAF agentic AI infrastructure

This directory contains tools and agents for AI use with DriftlessAF.

They can be used independently but are best used together with the DriftlessAF
workqueue found in `/workqueue` and the reconciler bots found in `/reconcilers`.

The following components are available:

- **AI executors**: Production-ready executors for Google Gemini and Anthropic
  Claude models in   `agents/executor/`.
- **Evaluation framework**: Testing and monitoring agent quality with
  comprehensive metrics in `agents/evals/`.
- **OpenTelemetry metrics**: Built-in observability for AI operations in
  `agents/metrics/`.
- **Prompt building**: Utilities for constructing and managing prompts in
  `agents/promptbuilder/`.
- **Tool calling**: Helpers for function/tool calling with Claude and Gemini
  in`agents/toolcall/`.
- **Result parsing**: Structured output extraction from model responses in
  `agents/result/`.
