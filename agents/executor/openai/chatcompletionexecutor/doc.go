/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package chatcompletionexecutor provides a multi-turn conversation executor for
// OpenAI-compatible chat completion APIs, including Vertex AI's partner model
// endpoint.
//
// Prefer [responsesexecutor] for providers that support the Responses API.
// Use this package for providers that require Chat Completions. See the
// [Responses executor documentation] for capabilities and configuration.
//
// The executor manages the full conversation lifecycle: sending prompts,
// processing tool calls, recording metrics, and extracting structured results.
// It mirrors the claudeexecutor and googleexecutor patterns, enabling the
// metaagent to route to any model available via the OpenAI chat completions API.
//
// # Basic Usage
//
//	client := openai.NewClient(
//		option.WithBaseURL(vertexBaseURL),
//		option.WithHTTPClient(authedClient),
//		option.WithAPIKey("placeholder"),
//	)
//
//	prompt := promptbuilder.MustParse("Analyze {{.Input}}")
//
//	exec, err := chatcompletionexecutor.New[MyRequest, MyResponse](client, prompt,
//		chatcompletionexecutor.WithModel[MyRequest, MyResponse]("deepseek-ai/deepseek-v3.2-maas"),
//		chatcompletionexecutor.WithMaxTokens[MyRequest, MyResponse](32768),
//		chatcompletionexecutor.WithTemperature[MyRequest, MyResponse](0.2),
//	)
//
// # Options
//
//   - [WithAttribution]: set canonical and compatibility route attribution
//   - [WithModel]: set the model name (required for Vertex AI partner models)
//   - [WithMaxTokens]: set the maximum completion tokens
//   - [WithTemperature]: set the sampling temperature (0.0–2.0)
//   - [WithoutTemperature]: omit temperature when an explicit route disallows sampling parameters
//   - [WithEffort]: set the reasoning effort for reasoning models (xhigh/max clamp to high)
//   - [WithMaxTurns]: set the maximum conversation turns before aborting
//   - [WithSystemInstructions]: set the system prompt
//   - [WithUserPromptSuffix]: append a static prompt to the built user prompt
//   - [WithSubmitResultProvider]: register the submit_result tool for structured output
//   - [WithRetryConfig]: configure retry behavior for transient API errors
//   - [WithResourceLabels]: set labels for observability attribution
//
// # Thinking Models
//
// Models that return reasoning_content in their responses (e.g. kimi-k2-thinking-maas)
// are supported. The executor captures reasoning content into the agent trace
// automatically.
//
// # Submit Result Redirect
//
// When a submit_result tool is configured but the model responds with text instead
// of calling the tool, the executor sends a redirect message asking the model to
// call submit_result. Unlike the claudeexecutor, the chatcompletionexecutor does not use
// a forced tool_choice for the redirect — some models (e.g. reasoning models)
// return 400 on named tool_choice constraints.
//
// [responsesexecutor]: https://pkg.go.dev/chainguard.dev/driftlessaf/agents/executor/openai/responsesexecutor
// [Responses executor documentation]: https://github.com/driftlessaf/go-driftlessaf/blob/main/agents/executor/openai/responsesexecutor/README.md
package chatcompletionexecutor
