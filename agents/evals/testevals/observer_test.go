/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testevals_test

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/testevals"
)

func TestTestingObserver(t *testing.T) {
	// Create a namespaced observer using testevals.NewPrefix
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return testevals.NewPrefix(t, name)
	})

	// Define a callback that uses the observer interface
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Starting trace analysis")

		if trace.InputPrompt == "" {
			o.Fail("Trace has empty input prompt")
			return
		}

		o.Log("Trace prompt: " + trace.InputPrompt)

		if trace.Error != nil {
			o.Fail("Trace error: " + trace.Error.Error())
			return
		}

		o.Log("Trace analysis complete")
	}

	// Inject the observer and pass to ByCode tracer
	traceCallback := evals.Inject[string](namespacedObs, callback)
	tracer := agenttrace.ByCode[string](traceCallback)

	// Create and complete a successful trace - this will automatically invoke the callback
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Test prompt")
	trace.Complete("test result", nil)
}

func TestTestingObserverWithError(t *testing.T) {
	// Create a namespaced observer using testevals.NewPrefix
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return testevals.NewPrefix(t, name)
	})

	// Define a callback that expects to find errors
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Checking for expected error")

		if trace.Error == nil {
			o.Fail("Expected trace to have an error")
			return
		}

		// This is expected, so we just log it
		o.Log("Found expected error: " + trace.Error.Error())
	}

	// Inject the observer and pass to ByCode tracer
	traceCallback := evals.Inject[string](namespacedObs, callback)
	tracer := agenttrace.ByCode[string](traceCallback)

	// Create and complete a trace with error - this will automatically invoke the callback
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Error test")
	trace.Complete("test result", errors.New("simulated error"))
}

func TestTestingObserverWithInject(t *testing.T) {
	// Create a namespaced observer using testevals.NewPrefix
	namespacedObs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return testevals.NewPrefix(t, name)
	})

	// Define a callback that validates tool calls
	validateCallback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Validating tool calls")

		if len(trace.ToolCalls) == 0 {
			o.Fail("Expected at least one tool call")
			return
		}

		for i, tc := range trace.ToolCalls {
			o.Log("Tool call " + string(rune('0'+i)) + ": " + tc.Name)
		}
	}

	// Inject the observer and pass to ByCode tracer
	traceCallback := evals.Inject(namespacedObs, validateCallback)
	tracer := agenttrace.ByCode[string](traceCallback)

	// Create a trace with tool calls using proper tracer
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, "Analyze logs")

	// Add tool calls
	tc1 := trace.StartToolCall("tc1", "read_logs", nil)
	tc1.Complete("log data", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", nil)
	tc2.Complete("analysis result", nil)

	// Complete the trace - this will automatically invoke the callback
	trace.Complete("analysis complete", nil)
}
