/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package evals provides a comprehensive tracing framework for evaluating and monitoring agent interactions.

# Overview

The evals package enables detailed tracking of agent execution flows, including prompts, tool calls,
results, and timing information. It provides a structured approach to capture evaluation data for
analysis, debugging, and performance monitoring of AI agents.

# Core Components

The package is built around several key types:

  - Tracer[T]: Generic interface for creating and managing traces with result type T
  - Trace[T]: Complete agent interaction from prompt to result of type T
  - ToolCall[T]: Individual tool invocation within a trace of type T
  - Observer: Interface for evaluation observing and grading
  - ObservableTraceCallback: Function type for trace evaluation callbacks
  - NamespacedObserver: Hierarchical namespace management for evaluations
  - ResultCollector: Observer wrapper that collects failure messages and grades
  - Grade: Structured grade with score and reasoning

# Generic Type Parameters

All core types are generic with type parameter T that serves two purposes:

1. **Type Safety**: The Result field in Trace[T] is strongly typed as T instead of interface{}
2. **Context Disambiguation**: Multiple tracers with different result types can coexist in the same context

**Important**: Only Trace.Result is generic (type T). ToolCall.Result remains interface{}
for maximum flexibility, as individual tool calls may return varied data types.

## Type Parameter Usage Patterns

### Simple Text Results
For basic string results from agent interactions:

	tracer := evals.ByCode[string]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Generate summary")
	trace.Complete("Summary: The analysis shows...", nil)

### Structured Results
For complex, type-safe results using custom structs:

	type AnalysisResult struct {
		TotalFiles   int     `json:"total_files"`
		IssuesFound  int     `json:"issues_found"`
		Confidence   float64 `json:"confidence"`
	}

	tracer := evals.ByCode[AnalysisResult]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze codebase")
	trace.Complete(AnalysisResult{
		TotalFiles:  42,
		IssuesFound: 3,
		Confidence:  0.95,
	}, nil)

### Multiple Tracers with Different Types
The same context can hold tracers for different result types:

	ctx := context.Background()

	// String tracer for text summaries
	stringTracer := evals.ByCode[string](stringCallback)
	ctx = evals.WithTracer[string](ctx, stringTracer)

	// Structured tracer for metrics
	metricsTracer := evals.ByCode[MetricsData](metricsCallback)
	ctx = evals.WithTracer[MetricsData](ctx, metricsTracer)

	// Both coexist without conflict
	summaryTrace := evals.StartTrace[string](ctx, "Generate summary")
	metricsTrace := evals.StartTrace[MetricsData](ctx, "Collect metrics")

**Note**: While Trace[interface{}] provides maximum flexibility when result types vary at runtime, prefer specific types when possible for better type safety and API clarity.

# Features

  - Thread-safe trace and tool call recording
  - Automatic trace completion and recording
  - Flexible callback system for custom trace processing
  - Context-based tracer management
  - Structured trace output with timing information
  - Support for both successful and failed tool calls
  - Concurrent execution support with proper synchronization
  - Built-in validation helpers for common evaluation patterns
  - Observer interface for test integration and result collection
  - Hierarchical namespacing for organized evaluation reporting
  - Integration with Go's testing framework

# Usage Patterns

## Basic Trace Creation

All traces must be created using a tracer. The simplest approach uses ByCode with no callbacks:

	tracer := evals.ByCode[string]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze the security report")
	toolCall := trace.StartToolCall("tc1", "file-reader", map[string]interface{}{
		"path": "/var/logs/security.log",
	})
	toolCall.Complete("File content here", nil)
	trace.Complete("Analysis complete", nil)

## Context-Based Tracing

For more sophisticated scenarios, use context-managed tracers:

	ctx := context.Background()
	tracer := evals.ByCode[string](func(trace *evals.Trace[string]) {
		log.Printf("Trace completed: %s", trace.ID)
	})
	ctx = evals.WithTracer[string](ctx, tracer)

	trace := evals.StartTrace[string](ctx, "Process user request")
	// ... perform operations
	trace.Complete("Request processed", nil)

## Custom Evaluation Callbacks

Create custom tracers with callback functions for specialized evaluation:

	tracer := evals.ByCode[string](
		func(trace *evals.Trace[string]) {
			// Save to database
			saveTraceToDatabase(trace)
		},
		func(trace *evals.Trace[string]) {
			// Send metrics
			recordMetrics(trace.Duration(), len(trace.ToolCalls))
		},
	)

## Evaluation Helpers

The package provides built-in validation helpers for common evaluation patterns.
All helper functions require explicit type parameters matching your trace result type:

	// Validate exact number of tool calls
	callback := evals.Inject[string](observer, evals.ExactToolCalls[string](2))

	// Validate no errors occurred
	callback = evals.Inject[string](observer, evals.NoErrors[string]())

	// Validate required tool usage
	callback = evals.Inject[string](observer, evals.RequiredToolCalls[string]([]string{"search", "analyze"}))

	// Custom tool call validation
	callback = evals.Inject[string](observer, evals.ToolCallValidator[string](func(o evals.Observer, tc *evals.ToolCall[string]) error {
		if tc.Name == "search" && tc.Result == nil {
			return fmt.Errorf("search tool must return results")
		}
		return nil
	}))

## Result Collection

Use ResultCollector to collect failure messages and grades from evaluations:

	// Create a base observer (could be namespaced)
	baseObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return customObserver(name)
	})

	// Wrap with result collector to capture evaluation outcomes
	collector := evals.NewResultCollector(baseObs)

	// Use in evaluation callbacks
	callback := func(o evals.Observer, trace *evals.Trace[string]) {
		if len(trace.ToolCalls) == 0 {
			o.Fail("No tool calls found")
		}
		o.Grade(0.85, "Good performance")
	}

	// Run evaluation
	tracer := evals.ByCode[string](evals.Inject[string](collector, callback))
	// ... create and complete traces

	// Collect results
	failures := collector.Failures()  // []string of failure messages
	grades := collector.Grades()      // []Grade with scores and reasoning

## Observer and Namespaced Evaluation

Use the NamespacedObserver for hierarchical evaluation organization:

	// Create a custom observer implementation
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return customObserver(name)  // your custom implementation
	})

	// Use with evaluation helpers in organized namespaces
	tracer := evals.ByCode[string](
		evals.Inject[string](namespacedObs.Child("accuracy"), evals.ExactToolCalls[string](1)),
		evals.Inject[string](namespacedObs.Child("reliability"), evals.NoErrors[string]()),
	)

# Integration Patterns

## Default Logging Integration

The package integrates with chainguard-dev/clog for structured logging:

	ctx := context.Background()
	tracer := evals.NewDefaultTracer[string](ctx) // Uses clog from context
	trace := tracer.NewTrace(ctx, "Execute workflow")

## Error Handling

The package handles both tool-level and trace-level errors:

	// Tool call that fails
	toolCall := trace.StartToolCall("tc1", "api-call", params)
	toolCall.Complete(nil, errors.New("API timeout"))

	// Bad tool call (invalid parameters)
	trace.BadToolCall("tc2", "unknown-tool", badParams, errors.New("unknown tool"))

	// Trace that fails
	trace.Complete(nil, errors.New("workflow failed"))

# Thread Safety

All operations are thread-safe. Multiple goroutines can safely:
  - Create and complete tool calls concurrently
  - Access trace duration and other methods
  - Record traces through tracer callbacks

The package uses fine-grained locking to ensure data consistency while maintaining performance.

# Performance Considerations

  - Trace IDs are generated with timestamp and randomness for uniqueness
  - Tool call and trace durations are calculated efficiently
  - String representations limit output size to prevent memory issues
  - Callbacks are executed in parallel using errgroup for better performance
*/
package evals
