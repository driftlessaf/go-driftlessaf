/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"chainguard.dev/driftlessaf/agents/evals"
)

// exampleObserver implements Observer for examples with thread-safety
type exampleObserver struct {
	failures []string
	logs     []string
	count    int64
	mu       sync.Mutex
}

func (m *exampleObserver) Fail(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = append(m.failures, msg)
}

func (m *exampleObserver) Log(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, msg)
}

func (m *exampleObserver) Grade(score float64, reasoning string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (m *exampleObserver) Increment() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
}

func (m *exampleObserver) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func ExampleResultCollector() {
	// Create a mock observer to demonstrate the pattern
	baseObs := &exampleObserver{}

	// Wrap it with a result collector
	collector := evals.NewResultCollector(baseObs)

	// Define an evaluation callback that validates tool calls
	callback := func(o evals.Observer, trace *evals.Trace[string]) {
		o.Log("Analyzing trace")

		if len(trace.ToolCalls) != 1 {
			o.Fail("Expected exactly 1 tool call")
		}

		if trace.Error != nil {
			o.Fail("Unexpected error: " + trace.Error.Error())
		}

		// Give the trace a grade
		o.Grade(0.85, "Good tool usage")
	}

	// Create tracer with the collector
	tracer := evals.ByCode[string](evals.Inject(collector, callback))

	// Create a trace that will trigger the evaluation
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Process data")

	// Add a tool call
	tc := trace.StartToolCall("tc1", "data-processor", map[string]any{
		"input": "some data",
	})
	tc.Complete("processed", nil)

	// Complete the trace (this triggers the evaluation)
	trace.Complete("Processing complete", nil)

	// Check collected results
	failures := collector.Failures()
	grades := collector.Grades()

	fmt.Printf("Failures: %d\n", len(failures))
	fmt.Printf("Grades: %d (score: %.2f)\n", len(grades), grades[0].Score)
	// Output: Failures: 0
	// Grades: 1 (score: 0.85)
}

func ExampleResultCollector_withNamespacedObserver() {
	// Create a namespaced observer using mock observers
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return &exampleObserver{}
	})

	// Create result collectors for different namespaces
	toolCollector := evals.NewResultCollector(namespacedObs.Child("tools"))
	errorCollector := evals.NewResultCollector(namespacedObs.Child("errors"))

	// Define evaluations for tool calls
	toolEval := func(o evals.Observer, trace *evals.Trace[string]) {
		for _, tc := range trace.ToolCalls {
			if tc.Error != nil {
				o.Fail(fmt.Sprintf("Tool %s failed: %v", tc.Name, tc.Error))
			}
		}
	}

	// Define evaluations for trace errors
	errorEval := func(o evals.Observer, trace *evals.Trace[string]) {
		if trace.Error != nil {
			o.Fail("Trace error: " + trace.Error.Error())
		}
	}

	// Create tracer with multiple collectors
	tracer := evals.ByCode[string](
		evals.Inject(toolCollector, toolEval),
		evals.Inject(errorCollector, errorEval),
	)

	// Create a trace with a failing tool call
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Complex analysis")

	tc := trace.StartToolCall("tc1", "analyzer", nil)
	tc.Complete(nil, errors.New("analysis failed"))

	// Complete the trace (this triggers both evaluations)
	trace.Complete("Analysis complete", nil)

	// Check failures by category
	toolFailures := toolCollector.Failures()
	errorFailures := errorCollector.Failures()

	fmt.Printf("Tool failures: %d\n", len(toolFailures))
	fmt.Printf("Error failures: %d\n", len(errorFailures))
	// Output: Tool failures: 1
	// Error failures: 0
}
