/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package claudetool provides utilities for handling tool calls in Claude AI applications.

This package simplifies the interaction with Claude's tool use feature by providing
type-safe parameter extraction and error handling utilities. It is designed to work
with the Anthropic Claude SDK and provides a consistent interface for tool implementations.

# Overview

The claudetool package offers the following key features:

  - Type-safe parameter extraction from Claude tool use blocks
  - Consistent error response formatting
  - Support for optional parameters with default values
  - Automatic type conversion for common JSON numeric types
  - Thread-safe operations

# Basic Usage

When implementing a tool for Claude, you typically need to extract parameters from
the tool use block and handle errors appropriately. This package simplifies that process:

	func handleReadFileTool(toolUse anthropic.ToolUseBlock) map[string]interface{} {
		// Create a parameter extractor
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			return errResp
		}

		// Extract required parameters
		filename, errResp := claudetool.Param[string](params, "filename")
		if errResp != nil {
			return errResp
		}

		// Extract optional parameters with defaults
		encoding, errResp := claudetool.OptionalParam(params, "encoding", "utf-8")
		if errResp != nil {
			return errResp
		}

		// Perform the tool operation
		content, err := readFile(filename, encoding)
		if err != nil {
			return claudetool.Error("Failed to read file %s: %v", filename, err)
		}

		return map[string]interface{}{
			"content": content,
		}
	}

# Parameter Extraction

The package provides two main functions for parameter extraction:

1. Param - Extracts required parameters:

	// Extract a required string parameter
	name, errResp := claudetool.Param[string](params, "name")

	// Extract a required integer (handles JSON float64 to int conversion)
	count, errResp := claudetool.Param[int](params, "count")

	// Extract complex types
	data, errResp := claudetool.Param[map[string]interface{}](params, "data")

2. OptionalParam - Extracts optional parameters with defaults:

	// Provide a default value if parameter is missing
	limit, errResp := claudetool.OptionalParam(params, "limit", 10)

	// Works with any type
	enabled, errResp := claudetool.OptionalParam(params, "enabled", true)

# Error Handling

The package provides consistent error response formatting that Claude expects:

	// Simple error message
	return claudetool.Error("File not found: %s", filename)

	// Error with additional context
	return claudetool.ErrorWithContext(err, map[string]interface{}{
		"filename": filename,
		"line": lineNumber,
	})

# Type Conversions

The package automatically handles common JSON numeric conversions:

	// JSON numbers are float64, but you can extract as int/int32/int64
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{"count": 42}`),
	}

	params, _ := claudetool.NewParams(toolUse)

	// All of these work correctly:
	asInt, _ := claudetool.Param[int](params, "count")       // 42
	asInt32, _ := claudetool.Param[int32](params, "count")   // 42
	asInt64, _ := claudetool.Param[int64](params, "count")   // 42
	asFloat, _ := claudetool.Param[float64](params, "count") // 42.0

# Thread Safety

All functions in this package are thread-safe. The Params type is immutable after
creation, making it safe to use concurrently.

# Integration with Claude

This package is designed to work seamlessly with Claude's tool use feature:

	tools := []anthropic.ToolUnionParam{{
		Name:        "read_file",
		Description: "Read the contents of a file",
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]interface{}{
				"filename": map[string]interface{}{
					"type":        "string",
					"description": "Path to the file to read",
				},
				"encoding": map[string]interface{}{
					"type":        "string",
					"description": "File encoding (optional)",
				},
			},
			Required: []string{"filename"},
		},
	}}

	// In your tool execution handler:
	switch toolUse.Name {
	case "read_file":
		return handleReadFileTool(toolUse)
	}
*/
package claudetool
