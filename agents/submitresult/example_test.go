/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult_test

import (
	"errors"
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

func ExampleResponsesTool() {
	type Result struct {
		Summary string `json:"summary"`
	}
	tool, err := submitresult.ResponsesTool(submitresult.OptionsForResponse[Result]())
	if err != nil {
		panic(err)
	}
	fmt.Println(tool.Definition.Name)
	// Output: submit_result
}

// ExampleErrParameter demonstrates how a consumer recognizes a submit
// rejected before its payload parsed. Match the class through the sentinel;
// the wrapped cause is for humans reading the trace, and quotes
// model-controlled text.
func ExampleErrParameter() {
	recorded := fmt.Errorf("%w: %w", submitresult.ErrParameter,
		errors.New("result parameter must be a JSON object, got string"))

	fmt.Println(errors.Is(recorded, submitresult.ErrParameter))
	fmt.Println(recorded)
	// Output:
	// true
	// parameter error: result parameter must be a JSON object, got string
}
