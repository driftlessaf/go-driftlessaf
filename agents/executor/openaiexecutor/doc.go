/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package openaiexecutor provides a multi-turn conversation executor for
// OpenAI-compatible chat completion APIs, including Vertex AI's partner model
// endpoint.
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
//	exec, err := openaiexecutor.New[MyRequest, MyResponse](client, prompt,
//		openaiexecutor.WithModel[MyRequest, MyResponse]("deepseek-ai/deepseek-v3.2-maas"),
//		openaiexecutor.WithMaxTokens[MyRequest, MyResponse](32768),
//		openaiexecutor.WithTemperature[MyRequest, MyResponse](0.2),
//	)
//
// # Options
//
//   - [WithModel]: set the model name (required for Vertex AI partner models)
//   - [WithMaxTokens]: set the maximum completion tokens
//   - [WithTemperature]: set the sampling temperature (0.0–2.0)
//   - [WithMaxTurns]: set the maximum conversation turns before aborting
//   - [WithSystemInstructions]: set the system prompt
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
// call submit_result. Unlike the claudeexecutor, the openaiexecutor does not use
// a forced tool_choice for the redirect — some models (e.g. reasoning models)
// return 400 on named tool_choice constraints.
package openaiexecutor
