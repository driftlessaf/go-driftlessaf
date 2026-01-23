/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool_test

import (
	"encoding/json"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// ExampleNewParams demonstrates how to create a parameter extractor from a Claude tool use block.
func ExampleNewParams() {
	// Simulate a tool use block from Claude
	toolUse := anthropic.ToolUseBlock{
		ID:   "tool_123",
		Name: "get_weather",
		Input: json.RawMessage(`{
			"location": "San Francisco",
			"units": "celsius"
		}`),
	}

	// Create parameter extractor
	params, errResp := claudetool.NewParams(toolUse)
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp["error"])
		return
	}

	// Extract location parameter
	location, _ := params.Get("location")
	fmt.Printf("Location: %v\n", location)

	// Output:
	// Location: San Francisco
}

// ExampleParam demonstrates extracting required parameters with type safety.
func ExampleParam() {
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{
			"filename": "data.txt",
			"line_number": 42,
			"verbose": true
		}`),
	}

	params, _ := claudetool.NewParams(toolUse)

	// Extract string parameter
	filename, errResp := claudetool.Param[string](params, "filename")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp)
		return
	}

	// Extract integer parameter (automatic conversion from float64)
	lineNumber, errResp := claudetool.Param[int](params, "line_number")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp)
		return
	}

	// Extract boolean parameter
	verbose, errResp := claudetool.Param[bool](params, "verbose")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp)
		return
	}

	fmt.Printf("File: %s, Line: %d, Verbose: %v\n", filename, lineNumber, verbose)

	// Output:
	// File: data.txt, Line: 42, Verbose: true
}

// ExampleParam_missingParameter demonstrates error handling for missing required parameters.
func ExampleParam_missingParameter() {
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{"other": "value"}`),
	}

	params, _ := claudetool.NewParams(toolUse)

	// Try to extract a missing parameter
	_, errResp := claudetool.Param[string](params, "filename")
	if errResp != nil {
		fmt.Printf("Error: %v\n", errResp["error"])
	}

	// Output:
	// Error: filename parameter is required
}

// ExampleOptionalParam demonstrates extracting optional parameters with default values.
func ExampleOptionalParam() {
	toolUse := anthropic.ToolUseBlock{
		Input: json.RawMessage(`{
			"message": "Hello"
		}`),
	}

	params, _ := claudetool.NewParams(toolUse)

	// Extract existing parameter (overrides default)
	message, _ := claudetool.OptionalParam(params, "message", "Default message")
	fmt.Printf("Message: %s\n", message)

	// Extract missing parameter (uses default)
	count, _ := claudetool.OptionalParam(params, "count", 10)
	fmt.Printf("Count: %d\n", count)

	// Extract missing boolean (uses default)
	enabled, _ := claudetool.OptionalParam(params, "enabled", true)
	fmt.Printf("Enabled: %v\n", enabled)

	// Output:
	// Message: Hello
	// Count: 10
	// Enabled: true
}

// ExampleError demonstrates creating simple error responses.
func ExampleError() {
	// Simple error
	errResp := claudetool.Error("File not found")
	fmt.Printf("Simple error: %v\n", errResp)

	// Formatted error
	filename := "data.txt"
	errResp = claudetool.Error("Cannot read file %s: permission denied", filename)
	fmt.Printf("Formatted error: %v\n", errResp["error"])

	// Output:
	// Simple error: map[error:File not found]
	// Formatted error: Cannot read file data.txt: permission denied
}

// ExampleErrorWithContext demonstrates creating error responses with additional context.
func ExampleErrorWithContext() {
	// Simulate an error condition
	err := errors.New("file not found")

	// Create error response with context
	errResp := claudetool.ErrorWithContext(err, map[string]any{
		"filename": "data.txt",
		"path":     "/var/data",
		"attempts": 3,
	})

	// The response includes both the error and context
	fmt.Printf("Error: %v\n", errResp["error"])
	fmt.Printf("Filename: %v\n", errResp["filename"])
	fmt.Printf("Path: %v\n", errResp["path"])
	fmt.Printf("Attempts: %v\n", errResp["attempts"])

	// Output:
	// Error: file not found
	// Filename: data.txt
	// Path: /var/data
	// Attempts: 3
}

// ExampleParams_completeToolImplementation demonstrates a complete tool implementation.
func ExampleParams_completeToolImplementation() {
	// Simulate a tool call from Claude for reading a file
	toolUse := anthropic.ToolUseBlock{
		ID:   "tool_456",
		Name: "read_file",
		Input: json.RawMessage(`{
			"filename": "example.txt",
			"encoding": "utf-8",
			"max_lines": 100
		}`),
	}

	// Function to handle the tool call
	handleReadFile := func(toolUse anthropic.ToolUseBlock) map[string]any {
		// Create parameter extractor
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			return errResp
		}

		// Extract required filename
		filename, errResp := claudetool.Param[string](params, "filename")
		if errResp != nil {
			return errResp
		}

		// Extract optional parameters
		encoding, errResp := claudetool.OptionalParam(params, "encoding", "utf-8")
		if errResp != nil {
			return errResp
		}

		maxLines, errResp := claudetool.OptionalParam(params, "max_lines", 1000)
		if errResp != nil {
			return errResp
		}

		// Simulate reading the file (in real code, you'd actually read the file)
		if filename == "nonexistent.txt" {
			return claudetool.ErrorWithContext(
				errors.New("file not found"),
				map[string]any{
					"filename": filename,
					"encoding": encoding,
				},
			)
		}

		// Return successful response
		return map[string]any{
			"content":    "File contents here...",
			"filename":   filename,
			"encoding":   encoding,
			"lines_read": 42,
			"max_lines":  maxLines,
		}
	}

	// Handle the tool call
	response := handleReadFile(toolUse)
	fmt.Printf("Response: %+v\n", response)

	// Output:
	// Response: map[content:File contents here... encoding:utf-8 filename:example.txt lines_read:42 max_lines:100]
}
