/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package testevals_test

import (
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

	// Create and complete a successful trace via done callback, which records and invokes the callback
	ctx := agenttrace.WithTracer[string](t.Context(), tracer)
	_, done := agenttrace.StartTrace[string](ctx, "Test prompt")
	done("test result", nil)
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

	// Create and complete a trace with error via done callback, which records and invokes the callback
	ctx := agenttrace.WithTracer[string](t.Context(), tracer)
	_, done := agenttrace.StartTrace[string](ctx, "Error test")
	done("test result", errors.New("simulated error"))
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
	ctx := agenttrace.WithTracer[string](t.Context(), tracer)
	trace, done := agenttrace.StartTrace[string](ctx, "Analyze logs")

	// Add tool calls
	tc1 := trace.StartToolCall("tc1", "read_logs", nil)
	tc1.Complete("log data", nil)

	tc2 := trace.StartToolCall("tc2", "analyze", nil)
	tc2.Complete("analysis result", nil)

	// Complete the trace via done callback, which records and invokes the callback
	done("analysis complete", nil)
}
