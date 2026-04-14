/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
)

// testObserver implements Observer for testing
type testObserver struct {
	failed  bool
	failMsg string
	logs    []string
	count   int64
}

func (m *testObserver) Fail(msg string) {
	m.failed = true
	m.failMsg = msg
}

func (m *testObserver) Log(msg string) {
	m.logs = append(m.logs, msg)
}

func (m *testObserver) Grade(score float64, reasoning string) {
	m.logs = append(m.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (m *testObserver) Increment() {
	m.count++
}

func (m *testObserver) Total() int64 {
	return m.count
}

func TestObservableTraceCallback(t *testing.T) {
	// Create a mock observer
	obs := &testObserver{}

	// Define a callback using the mock
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Processing trace")
		if trace.Error != nil {
			o.Fail("Trace had an error: " + trace.Error.Error())
		}
		o.Log("Trace processed")
	}

	// Inject the observer and pass to ByCode tracer
	traceCallback := evals.Inject[string](obs, callback)
	tracer := agenttrace.ByCode[string](traceCallback)
	ctx := agenttrace.WithTracer[string](t.Context(), tracer)

	// Create and complete a trace - done() records via the tracer, invoking the callback
	_, done := agenttrace.StartTrace[string](ctx, "Test prompt")
	done("test result", nil)

	// Verify the results
	if obs.failed {
		t.Errorf("failure: got = %s, wanted = none", obs.failMsg)
	}

	if len(obs.logs) != 2 {
		t.Errorf("log entry count: got = %d, wanted = 2", len(obs.logs))
	}

	if obs.logs[0] != "Processing trace" {
		t.Errorf("first log: got = %s, wanted = 'Processing trace'", obs.logs[0])
	}

	if obs.logs[1] != "Trace processed" {
		t.Errorf("second log: got = %s, wanted = 'Trace processed'", obs.logs[1])
	}
}

func TestObservableTraceCallbackWithError(t *testing.T) {
	// Create a mock observer
	obs := &testObserver{}

	// Define an ObservableTraceCallback that checks for errors
	callback := func(o evals.Observer, trace *agenttrace.Trace[string]) {
		o.Log("Checking trace")
		if trace.Error != nil {
			o.Fail("Trace error: " + trace.Error.Error())
			return
		}
		o.Log("Trace is good")
	}

	// Inject the observer and pass to ByCode tracer
	traceCallback := evals.Inject[string](obs, callback)
	tracer := agenttrace.ByCode[string](traceCallback)
	ctx := agenttrace.WithTracer[string](t.Context(), tracer)

	// Create and complete a trace with an error - done() records via the tracer, invoking the callback
	_, done := agenttrace.StartTrace[string](ctx, "Test prompt")
	done("test result", errors.New("test error"))

	// Verify the results
	if !obs.failed {
		t.Error("obs.failed: got = false, wanted = true")
	}

	if !strings.Contains(obs.failMsg, "Trace error:") {
		t.Errorf("failure message: got = %s, wanted = contains 'Trace error:'", obs.failMsg)
	}

	if len(obs.logs) != 1 {
		t.Errorf("log entry count: got = %d, wanted = 1", len(obs.logs))
	}

	if obs.logs[0] != "Checking trace" {
		t.Errorf("log message: got = %s, wanted = 'Checking trace'", obs.logs[0])
	}
}
