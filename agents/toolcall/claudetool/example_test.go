/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudetool_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
)

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

// ExampleFromTool demonstrates converting a unified tool to Claude-specific metadata.
func ExampleFromTool() {
	// Define a unified tool that works with any provider.
	tool := toolcall.Tool[string]{
		Def: toolcall.Definition{
			Name:        "greet",
			Description: "Greet a person by name.",
			Parameters: []toolcall.Parameter{{
				Name:        "name",
				Type:        "string",
				Description: "The name of the person to greet.",
				Required:    true,
			}},
		},
		Handler: func(_ context.Context, call toolcall.ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
			name, _ := call.Args["name"].(string)
			return map[string]any{"greeting": "Hello, " + name + "!"}
		},
	}

	// Convert the unified tool to Claude-specific metadata.
	meta := claudetool.FromTool(tool)
	fmt.Println(meta.Definition.Name)

	// Output:
	// greet
}

// ExampleMap demonstrates converting a map of unified tools to Claude-specific metadata.
func ExampleMap() {
	// Define a map of unified tools.
	tools := map[string]toolcall.Tool[string]{
		"greet": {
			Def: toolcall.Definition{
				Name:        "greet",
				Description: "Greet a person by name.",
			},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
				return map[string]any{"greeting": "Hello!"}
			},
		},
		"farewell": {
			Def: toolcall.Definition{
				Name:        "farewell",
				Description: "Say farewell to a person.",
			},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[string], _ *string) map[string]any {
				return map[string]any{"farewell": "Goodbye!"}
			},
		},
	}

	// Convert all tools to Claude-specific metadata.
	meta := claudetool.Map(tools)
	fmt.Println(len(meta))

	// Output:
	// 2
}
