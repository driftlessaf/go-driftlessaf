/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
)

// ExampleExactToolCalls shows how to validate exact tool call counts
func ExampleExactToolCalls() {
	// Create a mock observer
	obs := &mockObserver{}

	// Use ExactToolCalls to validate exactly 2 tool calls
	evalCallback := evals.ExactToolCalls[string](2)

	// Create tracer with the evaluation
	tracer := agenttrace.ByCode[string](evals.Inject(obs, evalCallback))

	// Create trace with exactly 2 tool calls
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze logs")

	// Add exactly 2 tool calls
	tc1 := trace.StartToolCall("tc1", "read_logs", nil)
	tc1.Complete("log data", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", nil)
	tc2.Complete("analysis done", nil)

	// Complete trace (triggers evaluation)
	trace.Complete("Analysis complete", nil)

	if len(obs.failures) == 0 {
		fmt.Println("Validation passed: exactly 2 tool calls")
	}

	// Output:
	// Validation passed: exactly 2 tool calls
}

// ExampleRequiredToolCalls shows how to ensure specific tools are called
func ExampleRequiredToolCalls() {
	// Create a mock observer
	obs := &mockObserver{}

	// Require both read_logs and analyze to be called
	evalCallback := evals.RequiredToolCalls[string]([]string{"read_logs", "analyze"})

	// Create tracer with the evaluation
	tracer := agenttrace.ByCode[string](evals.Inject(obs, evalCallback))

	// Create trace and add required tools (plus extra)
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Process data")

	// Add required tools
	tc1 := trace.StartToolCall("tc1", "read_logs", nil)
	tc1.Complete("log data", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", nil)
	tc2.Complete("analysis done", nil)

	// Add extra tool (should be fine)
	tc3 := trace.StartToolCall("tc3", "summarize", nil)
	tc3.Complete("summary", nil)

	// Complete trace (triggers evaluation)
	trace.Complete("Processing complete", nil)

	if len(obs.failures) == 0 {
		fmt.Println("All required tools were called")
	}

	// Output:
	// All required tools were called
}

// ExampleToolCallValidator shows custom validation of tool calls
func ExampleToolCallValidator() {
	// Create a mock observer
	obs := &mockObserver{}

	// Validate that all tool calls have a reasoning parameter
	validator := func(o evals.Observer, tc *agenttrace.ToolCall[string]) error {
		if _, ok := tc.Params["reasoning"]; !ok {
			return errors.New("missing reasoning parameter")
		}
		return nil
	}

	evalCallback := evals.ToolCallValidator[string](validator)

	// Create tracer with the evaluation
	tracer := agenttrace.ByCode[string](evals.Inject(obs, evalCallback))

	// Create trace with proper reasoning parameters
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze logs")

	// Add tool calls with reasoning parameters
	tc1 := trace.StartToolCall("tc1", "read_logs", map[string]any{
		"reasoning": "need to analyze logs",
	})
	tc1.Complete("log data", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", map[string]any{
		"reasoning": "extract error patterns",
	})
	tc2.Complete("analysis done", nil)

	// Complete trace (triggers evaluation)
	trace.Complete("Analysis complete", nil)

	if len(obs.failures) == 0 {
		fmt.Println("All tool calls have reasoning")
	}

	// Output:
	// All tool calls have reasoning
}

// ExampleNoErrors shows how to validate no errors occurred
func ExampleNoErrors() {
	// Create a mock observer
	obs := &mockObserver{}

	evalCallback := evals.NoErrors[string]()

	// Create tracer with the evaluation
	tracer := agenttrace.ByCode[string](evals.Inject(obs, evalCallback))

	// Create successful trace with no errors
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Read and analyze")

	// Add successful tool calls
	tc1 := trace.StartToolCall("tc1", "read_logs", nil)
	tc1.Complete("log content", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", nil)
	tc2.Complete("analysis complete", nil)

	// Complete trace successfully (triggers evaluation)
	trace.Complete("Processing complete", nil)

	if len(obs.failures) == 0 {
		fmt.Println("No errors found")
	}

	// Output:
	// No errors found
}
