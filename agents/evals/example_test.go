/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/evals"
)

// ExampleTracer_NewTrace demonstrates basic trace creation and tool call tracking.
func ExampleTracer_NewTrace() {
	// Create a tracer and use it to create a trace
	tracer := evals.ByCode[string]() // No callbacks for this example
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze security vulnerabilities in the codebase")

	// Start a tool call to scan files
	toolCall := trace.StartToolCall("tc1", "file-scanner", map[string]any{
		"path":    "/src",
		"pattern": "*.go",
	})

	// Complete the tool call with results
	toolCall.Complete(map[string]any{
		"files_scanned": 42,
		"issues_found":  3,
	}, nil)

	// Start another tool call
	reportCall := trace.StartToolCall("tc2", "report-generator", map[string]any{
		"format": "json",
	})
	reportCall.Complete("Security report generated", nil)

	// Complete the overall trace
	trace.Complete("Security analysis completed successfully", nil)

	fmt.Printf("Trace completed with %d tool calls\n", len(trace.ToolCalls))
	// Output: Trace completed with 2 tool calls
}

// ExampleStartTrace demonstrates context-based trace management.
func ExampleStartTrace() {
	ctx := context.Background()

	// Create a custom tracer with a callback
	tracer := evals.ByCode[string](func(trace *evals.Trace[string]) {
		fmt.Printf("Trace %s completed in %v\n", trace.ID[:8], trace.Duration())
	})

	// Add tracer to context
	ctx = evals.WithTracer(ctx, tracer)

	// Start a trace using the context tracer
	trace := evals.StartTrace[string](ctx, "Process user authentication")

	// Start a tool call
	tc := trace.StartToolCall("call-1", "text-analyzer", map[string]any{
		"text": "I love sunny days!",
		"mode": "sentiment",
	})

	// Complete the tool call with result
	tc.Complete(map[string]any{
		"sentiment": "positive",
		"score":     0.95,
	}, nil)

	// Complete the trace with the final result (this will automatically record it)
	trace.Complete("Sentiment analysis complete: positive with 95% confidence", nil)
}

// ExampleByCode demonstrates creating a tracer with multiple callbacks.
func ExampleByCode() {
	// Create a code-based eval with multiple callbacks
	var traces []*evals.Trace[string]
	var toolCallCount int

	tracer := evals.ByCode[string](
		// First callback: collect traces
		func(trace *evals.Trace[string]) {
			traces = append(traces, trace)
		},
		// Second callback: count tool calls
		func(trace *evals.Trace[string]) {
			toolCallCount += len(trace.ToolCalls)
		},
		// Third callback: log completion
		func(trace *evals.Trace[string]) {
			fmt.Println("Trace completed")
		},
	)

	// Use the tracer
	ctx := context.Background()
	ctx = evals.WithTracer(ctx, tracer)

	// Simulate agent operations
	trace := evals.StartTrace[string](ctx, "Process data")
	tc := trace.StartToolCall("tool-1", "processor", nil)
	tc.Complete("done", nil)
	trace.Complete("Processed", nil)

	fmt.Printf("Total traces: %d, Total tool calls: %d\n", len(traces), toolCallCount)
	// Output: Trace completed
	// Total traces: 1, Total tool calls: 1
}

// ExampleTrace_BadToolCall demonstrates recording failed tool calls.
func ExampleTrace_BadToolCall() {
	tracer := evals.ByCode[string]() // No callbacks for this example
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Execute automated tasks")

	// Record a tool call that failed due to bad parameters
	trace.BadToolCall(
		"bad1",
		"unknown-tool",
		map[string]any{
			"invalid": "parameters",
		},
		fmt.Errorf("unknown tool: unknown-tool"),
	)

	// Continue with valid tool calls
	validCall := trace.StartToolCall("valid1", "file-reader", map[string]any{
		"path": "/etc/hosts",
	})
	validCall.Complete("file contents", nil)

	trace.Complete("Task completed with some failures", nil)

	fmt.Printf("Total tool calls: %d\n", len(trace.ToolCalls))
	fmt.Printf("First call had error: %v\n", trace.ToolCalls[0].Error != nil)
	// Output: Total tool calls: 2
	// First call had error: true
}

// ExampleTrace_StartToolCall demonstrates starting and completing tool calls.
func ExampleTrace_StartToolCall() {
	// Create a new trace using a tracer
	tracer := evals.ByCode[string]() // No callbacks for this example
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Generate a random number")

	// Start and complete a successful tool call
	tc1 := trace.StartToolCall("rnd-1", "random-generator",
		map[string]any{"min": 1, "max": 100})
	tc1.Complete(map[string]any{"value": 42}, nil)

	// Start and complete a failed tool call
	tc2 := trace.StartToolCall("net-1", "network-request",
		map[string]any{"url": "https://example.com/api"})
	tc2.Complete(nil, errors.New("connection timeout"))

	// Complete the trace
	trace.Complete("Generated number: 42", nil)

	fmt.Println("Trace completed successfully")
	// Output: Trace completed successfully
}

// ExampleNewDefaultTracer demonstrates the default logging tracer.
func ExampleNewDefaultTracer() {
	ctx := context.Background()

	// Create default tracer (uses clog for logging)
	tracer := evals.NewDefaultTracer[string](ctx)

	// Create and complete a trace
	trace := tracer.NewTrace(ctx, "System health check")

	healthCall := trace.StartToolCall("health1", "check-services", nil)
	healthCall.Complete("all services healthy", nil)

	// Completing this trace will log structured information
	trace.Complete("Health check passed", nil)

	fmt.Println("Health check trace completed")
	// Output: Health check trace completed
}

// ExampleTracerFromContext demonstrates retrieving tracers from context.
func ExampleTracerFromContext() {
	ctx := context.Background()

	// Get tracer from context (will return default tracer if none set)
	tracer1 := evals.TracerFromContext[string](ctx)
	fmt.Printf("Default tracer type: %T\n", tracer1)

	// Add custom tracer to context
	customTracer := evals.ByCode[string](func(*evals.Trace[string]) {
		fmt.Println("Custom callback executed")
	})
	ctx = evals.WithTracer(ctx, customTracer)

	// Retrieve the custom tracer
	tracer2 := evals.TracerFromContext[string](ctx)
	fmt.Printf("Custom tracer retrieved: %t\n", tracer2 == customTracer)
	// Output: Default tracer type: *evals.byCodeTracer[string]
	// Custom tracer retrieved: true
}
