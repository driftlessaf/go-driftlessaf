/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/submitresult"
)

// ExampleClaudeTool demonstrates constructing the Claude submit_result tool
// metadata for a custom response type.
func ExampleClaudeTool() {
	type MyResult struct {
		Summary string `json:"summary" jsonschema:"required,description=Summary of findings"`
	}

	tool, err := submitresult.ClaudeTool[*MyResult](submitresult.Options[*MyResult]{
		Description:        "Submit the final analysis result.",
		PayloadDescription: "Structured analysis result.",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("tool name:", tool.Definition.Name)
	// Output: tool name: submit_result
}
