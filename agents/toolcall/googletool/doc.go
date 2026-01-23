/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package googletool provides utilities for handling function calls in Google Gemini AI applications.

This package simplifies the interaction with Google's Generative AI function calling feature
by providing type-safe parameter extraction and error handling utilities. It is designed to
work with the Google Generative AI SDK and provides a consistent interface for function
implementations.

# Overview

The googletool package offers the following key features:

  - Type-safe parameter extraction from Gemini function calls
  - Consistent error response formatting
  - Support for optional parameters with default values
  - Automatic type conversion for common JSON numeric types
  - Thread-safe operations

# Basic Usage

When implementing a function for Gemini, you typically need to extract parameters from
the function call and handle errors appropriately. This package simplifies that process:

	func handleReadFileFunction(call *genai.FunctionCall) *genai.FunctionResponse {
		// Extract required parameters
		filename, errResp := googletool.Param[string](call, "filename")
		if errResp != nil {
			return errResp
		}

		// Extract optional parameters with defaults
		encoding, errResp := googletool.OptionalParam(call, "encoding", "utf-8")
		if errResp != nil {
			return errResp
		}

		// Perform the function operation
		content, err := readFile(filename, encoding)
		if err != nil {
			return googletool.Error(call, "Failed to read file %s: %v", filename, err)
		}

		return &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: map[string]interface{}{
				"content": content,
			},
		}
	}

# Parameter Extraction

The package provides two main functions for parameter extraction:

1. Param - Extracts required parameters:

	// Extract a required string parameter
	name, errResp := googletool.Param[string](call, "name")

	// Extract a required integer (handles JSON float64 to int conversion)
	count, errResp := googletool.Param[int](call, "count")

	// Extract complex types
	data, errResp := googletool.Param[map[string]interface{}](call, "data")

2. OptionalParam - Extracts optional parameters with defaults:

	// Provide a default value if parameter is missing
	limit, errResp := googletool.OptionalParam(call, "limit", 10)

	// Works with any type
	enabled, errResp := googletool.OptionalParam(call, "enabled", true)

# Error Handling

The package provides consistent error response formatting that Gemini expects:

	// Simple error message
	return googletool.Error(call, "File not found: %s", filename)

	// Error with additional context
	return googletool.ErrorWithContext(call, err, map[string]interface{}{
		"filename": filename,
		"line": lineNumber,
	})

# Type Conversions

The package automatically handles common JSON numeric conversions:

	// JSON numbers are float64, but you can extract as int/int32/int64
	call := &genai.FunctionCall{
		Name: "process",
		Args: map[string]interface{}{"count": 42.0},
	}

	// All of these work correctly:
	asInt, _ := googletool.Param[int](call, "count")       // 42
	asInt32, _ := googletool.Param[int32](call, "count")   // 42
	asInt64, _ := googletool.Param[int64](call, "count")   // 42
	asFloat, _ := googletool.Param[float64](call, "count") // 42.0

# Thread Safety

All functions in this package are thread-safe as they operate on immutable data
and don't maintain any shared state.

# Integration with Gemini

This package is designed to work seamlessly with Gemini's function calling feature:

	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "read_file",
			Description: "Read the contents of a file",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"filename": {
						Type:        "string",
						Description: "Path to the file to read",
					},
					"encoding": {
						Type:        "string",
						Description: "File encoding (optional)",
					},
				},
				Required: []string{"filename"},
			},
		}},
	}}

	// In your function execution handler:
	switch call.Name {
	case "read_file":
		response := handleReadFileFunction(call)
	}

# Complete Example

Here's a complete example of implementing a function that lists files in a directory:

	func handleListFilesFunction(call *genai.FunctionCall) *genai.FunctionResponse {
		// Extract required directory parameter
		directory, errResp := googletool.Param[string](call, "directory")
		if errResp != nil {
			return errResp
		}

		// Extract optional parameters
		pattern, errResp := googletool.OptionalParam(call, "pattern", "*")
		if errResp != nil {
			return errResp
		}

		recursive, errResp := googletool.OptionalParam(call, "recursive", false)
		if errResp != nil {
			return errResp
		}

		// Perform the operation
		files, err := listFiles(directory, pattern, recursive)
		if err != nil {
			return googletool.ErrorWithContext(call, err, map[string]interface{}{
				"directory": directory,
				"pattern":   pattern,
			})
		}

		// Return successful response
		return &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: map[string]interface{}{
				"files": files,
				"count": len(files),
			},
		}
	}
*/
package googletool
