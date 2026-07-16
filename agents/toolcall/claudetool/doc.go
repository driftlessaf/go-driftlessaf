/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package claudetool converts unified [toolcall.Tool] definitions into
// Claude-compatible [anthropic.ToolParam] metadata for use with the
// [claudeexecutor].
//
// This package mirrors [googletool] and [openaistool], providing tool
// conversion and error formatting for tool handlers that receive
// [anthropic.ToolUseBlock] values.
//
// # Tool Conversion
//
// Use [FromTool] to convert a single unified tool, or [Map] to batch-convert
// an entire tool map:
//
//	tools := claudetool.Map[MyResponse](unifiedTools)
//
// Each converted tool automatically injects a "reasoning" parameter that the
// model must fill in to explain its intent before making the call.
//
// # Error Formatting
//
// Use [Error] to build tool error responses:
//
//	return claudetool.Error("file not found: %s", path)
//
// # Thread Safety
//
// All functions in this package are safe for concurrent use.
package claudetool
