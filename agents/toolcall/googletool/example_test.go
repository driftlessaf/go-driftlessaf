/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googletool_test

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// ExampleError demonstrates creating error responses for function calls.
func ExampleError() {
	call := &genai.FunctionCall{
		ID:   "call_error",
		Name: "process_data",
	}

	// Simple error
	errResp := googletool.Error(call, "Invalid data format")
	fmt.Printf("Error ID: %s\n", errResp.ID)
	fmt.Printf("Error message: %v\n", errResp.Response["error"])

	// Formatted error
	filename := "data.csv"
	line := 42
	errResp = googletool.Error(call, "Parse error in %s at line %d", filename, line)
	fmt.Printf("Formatted error: %v\n", errResp.Response["error"])

	// Output:
	// Error ID: call_error
	// Error message: Invalid data format
	// Formatted error: Parse error in data.csv at line 42
}

// ExampleErrorWithContext demonstrates creating error responses with additional context.
func ExampleErrorWithContext() {
	call := &genai.FunctionCall{
		ID:   "call_context",
		Name: "upload_file",
		Args: map[string]any{
			"filename": "large_file.zip",
		},
	}

	// Simulate an error condition
	err := errors.New("file size exceeds limit")

	// Create error response with context
	errResp := googletool.ErrorWithContext(call, err, map[string]any{
		"filename":    "large_file.zip",
		"size_mb":     156.7,
		"limit_mb":    100,
		"retry_after": 3600,
	})

	// The response includes both the error and context
	fmt.Printf("Error: %v\n", errResp.Response["error"])
	fmt.Printf("Size: %.1f MB\n", errResp.Response["size_mb"])
	fmt.Printf("Limit: %d MB\n", errResp.Response["limit_mb"])

	// Output:
	// Error: file size exceeds limit
	// Size: 156.7 MB
	// Limit: 100 MB
}

// ExampleFromTool demonstrates converting a unified tool to Google-specific metadata.
func ExampleFromTool() {
	// Define a unified tool that works with any provider.
	type Result struct{ Summary string }

	t := toolcall.Tool[Result]{
		Def: toolcall.Definition{
			Name:        "summarize",
			Description: "Summarize the provided text.",
			Parameters: []toolcall.Parameter{{
				Name:        "text",
				Type:        "string",
				Description: "The text to summarize.",
				Required:    true,
			}},
		},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[Result], _ *Result) map[string]any {
			text, errResp := toolcall.OptionalParam(call, "text", "")
			if errResp != nil {
				return errResp
			}
			return map[string]any{"summary": "Summary of: " + text}
		},
	}

	// Convert the unified tool to Google-specific metadata.
	meta := googletool.FromTool(t)

	fmt.Printf("Name: %s\n", meta.Definition.Name)
	fmt.Printf("Description: %s\n", meta.Definition.Description)
	fmt.Printf("Handler set: %v\n", meta.Handler != nil)

	// Output:
	// Name: summarize
	// Description: Summarize the provided text.
	// Handler set: true
}

// ExampleMap demonstrates converting a map of unified tools to Google-specific metadata.
func ExampleMap() {
	// Define a set of unified tools.
	type Result struct{ Answer string }

	tools := map[string]toolcall.Tool[Result]{
		"greet": {
			Def: toolcall.Definition{
				Name:        "greet",
				Description: "Greet a user by name.",
				Parameters: []toolcall.Parameter{{
					Name:        "name",
					Type:        "string",
					Description: "The name of the user.",
					Required:    true,
				}},
			},
			Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[Result], _ *Result) map[string]any {
				return map[string]any{"greeting": "Hello!"}
			},
		},
		"farewell": {
			Def: toolcall.Definition{
				Name:        "farewell",
				Description: "Say farewell to a user.",
				Parameters:  []toolcall.Parameter{},
			},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[Result], _ *Result) map[string]any {
				return map[string]any{"message": "Goodbye!"}
			},
		},
	}

	// Convert the entire map to Google-specific metadata.
	meta := googletool.Map(tools)

	fmt.Printf("Tool count: %d\n", len(meta))
	fmt.Printf("greet handler set: %v\n", meta["greet"].Handler != nil)
	fmt.Printf("farewell handler set: %v\n", meta["farewell"].Handler != nil)

	// Output:
	// Tool count: 2
	// greet handler set: true
	// farewell handler set: true
}
