/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package googletool converts unified [toolcall.Tool] definitions into
// Gemini-compatible [genai.FunctionDeclaration] metadata for use with the
// [googleexecutor].
//
// This package mirrors [claudetool] and [openaistool], providing tool
// conversion and error formatting for tool handlers that receive
// [genai.FunctionCall] values.
//
// # Tool Conversion
//
// Use [FromTool] to convert a single unified tool, or [Map] to batch-convert
// an entire tool map:
//
//	tools := googletool.Map[MyResponse](unifiedTools)
//
// Each converted tool automatically injects a "reasoning" parameter that the
// model must fill in to explain its intent before making the call.
//
// # Error Formatting
//
// Use [Error] and [ErrorWithContext] to build [genai.FunctionResponse] error
// responses tagged with the originating call's ID and name:
//
//	return googletool.Error(call, "file not found: %s", path)
//
// # Thread Safety
//
// All functions in this package are safe for concurrent use.
package googletool
