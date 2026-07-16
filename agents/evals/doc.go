/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package evals provides observers and validation helpers for evaluating
completed agent traces.

Tracing itself lives in chainguard.dev/driftlessaf/agents/agenttrace; this
package layers evaluation on top. An evaluation is an ObservableTraceCallback —
a function receiving an Observer and a completed agenttrace.Trace[T] — that
reports outcomes through the Observer (Fail, Log, Grade, Increment, Total).

# Observers

  - NamespacedObserver: hierarchical namespaces ("accuracy", "reliability", ...)
    built from a factory function
  - ResultCollector: wraps another Observer and collects failure messages and
    Grades for later inspection
  - MetricsObserver: publishes evaluation counts, failures, and grades as
    Prometheus metrics
  - testevals.New / testevals.NewPrefix (subpackage): adapt *testing.T to the
    Observer interface

# Validation helpers

Ready-made ObservableTraceCallbacks cover common checks:

	// Tool call counts
	evals.ExactToolCalls[string](2)
	// (also MinimumNToolCalls, MaximumNToolCalls, RangeToolCalls, NoToolCalls)

	// Tool usage constraints
	evals.RequiredToolCalls[string]([]string{"search", "analyze"})
	evals.OnlyToolCalls[string]("search", "analyze")

	// No trace-level errors
	evals.NoErrors[string]()

	// Custom validation of tool calls or the trace result
	evals.ToolCallValidator[string](func(o evals.Observer, tc *agenttrace.ToolCall[string]) error { ... })
	evals.ToolCallNamed[string]("search", func(o evals.Observer, tc *agenttrace.ToolCall[string]) error { ... })
	evals.ResultValidator[string](func(result string) error { ... })

Additional helpers guard against known bad tool-call patterns:
NoReadFileOnDirectory, NoHallucinatedPaths, EditStringExists,
ValidRegexPattern.

# Wiring evaluations into a tracer

Inject binds an Observer to an ObservableTraceCallback, producing an
agenttrace.TraceCallback that runs when a trace completes:

	obs := evals.NewNamespacedObserver(func(name string) evals.Observer {
		return customObserver(name)
	})
	tracer := agenttrace.ByCode[string](
		evals.Inject[string](obs.Child("tool-calls"), evals.ExactToolCalls[string](1)),
		evals.Inject[string](obs.Child("reliability"), evals.NoErrors[string]()),
	)
	ctx = agenttrace.WithTracer[string](ctx, tracer)

BuildCallbacks and BuildTracer do the same for a map of named evaluations,
namespacing each entry under the observer. NewDefaultTracer returns a tracer
that logs completed traces via clog.

# Reporting

The report subpackage turns a NamespacedObserver[*ResultCollector] tree into
markdown evaluation reports.
*/
package evals
