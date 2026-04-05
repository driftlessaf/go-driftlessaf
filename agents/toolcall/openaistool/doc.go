/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package openaistool converts unified [toolcall.Tool] definitions into
// OpenAI-compatible [openai.ChatCompletionToolParam] metadata for use with
// the [openaiexecutor].
//
// This package mirrors [claudetool] and [googletool], providing type-safe
// parameter extraction and error formatting for tool handlers that receive
// [openai.ChatCompletionMessageToolCall] values.
//
// # Tool Conversion
//
// Use [FromTool] to convert a single unified tool, or [Map] to batch-convert
// an entire tool map:
//
//	tools := openaistool.Map[MyResponse](unifiedTools)
//
// Each converted tool automatically injects a "reasoning" parameter that the
// model must fill in to explain its intent before making the call.
//
// # Error Formatting
//
// Use [Error] and [ErrorWithContext] to build tool error responses:
//
//	return openaistool.Error("file not found: %s", path)
//
// # Thread Safety
//
// All functions in this package are safe for concurrent use.
package openaistool
