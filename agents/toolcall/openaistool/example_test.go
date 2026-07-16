/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaistool_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
)

type analysisResult struct {
	Summary string `json:"summary"`
}

func ExampleFromTool() {
	t := toolcall.Tool[analysisResult]{
		Def: toolcall.Definition{
			Name:        "read_file",
			Description: "Read the contents of a file.",
			Parameters: []toolcall.Parameter{
				{
					Name:        "path",
					Type:        "string",
					Description: "The file path to read.",
					Required:    true,
				},
			},
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[analysisResult], result *analysisResult) map[string]any {
			return map[string]any{"content": "file contents"}
		},
	}

	meta := openaistool.FromTool(t)
	fmt.Println(meta.Definition.Function.Name)
	// Output: read_file
}

func ExampleMap() {
	tools := map[string]toolcall.Tool[analysisResult]{
		"read_file": {
			Def: toolcall.Definition{
				Name:        "read_file",
				Description: "Read the contents of a file.",
				Parameters: []toolcall.Parameter{
					{
						Name:        "path",
						Type:        "string",
						Description: "The file path to read.",
						Required:    true,
					},
				},
			},
			Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[analysisResult], result *analysisResult) map[string]any {
				return map[string]any{"content": "file contents"}
			},
		},
	}

	converted := openaistool.Map(tools)
	fmt.Println(len(converted))
	// Output: 1
}

func ExampleError() {
	result := openaistool.Error("file not found: %q", "/etc/missing")
	fmt.Println(result["error"])
	// Output: file not found: "/etc/missing"
}
