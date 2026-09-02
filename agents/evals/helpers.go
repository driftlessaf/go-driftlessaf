/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/workqueue"
)

// ExactToolCalls returns an ObservableTraceCallback that validates the trace has exactly n tool calls.
func ExactToolCalls[T any](n int) ObservableTraceCallback[T] {
	return func(o Observer, trace *agenttrace.Trace[T]) {
		if got := len(trace.ToolCalls); got != n {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted = %d", got, n))
		}
	}
}

// MinimumNToolCalls returns an ObservableTraceCallback that validates the trace has at least n tool calls.
func MinimumNToolCalls[T any](n int) ObservableTraceCallback[T] {
	return func(o Observer, trace *agenttrace.Trace[T]) {
		if got := len(trace.ToolCalls); got < n {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted >= %d", got, n))
		}
	}
}

// RangeToolCalls returns an ObservableTraceCallback that validates the trace has between min and max tool calls (inclusive).
func RangeToolCalls[T any](min, max int) ObservableTraceCallback[T] {
	return func(o Observer, trace *agenttrace.Trace[T]) {
		if got := len(trace.ToolCalls); got < min || got > max {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted = %d..%d", got, min, max))
		}
	}
}

// NoToolCalls returns an ObservableTraceCallback that validates the trace has no tool calls.
func NoToolCalls[T any]() ObservableTraceCallback[T] {
	return ExactToolCalls[T](0)
}

// OnlyToolCalls returns an ObservableTraceCallback that validates the trace only uses the specified tool names.
//
// A call outside the allowed set whose error matches agenttrace.ErrUnknownTool
// is skipped rather than failed: the executor rejected the call, so it never
// ran. When at least one call was skipped, the metric grades 1.0 with a
// reasoning string naming the skipped calls, so the signal reaches score
// sinks. A call outside the set with no error, or with an error that does not
// match the sentinel, still fails.
//
// Skipping is justified only when the run demonstrably worked past the
// rejection. A success is a call with no error that is not the terminal
// submit, the same rule NoErrors applies: committing a result is not work.
// When calls were skipped and no call succeeded as work, the run's only
// activity was rejected invented calls plus the steered terminal submit, so
// the metric fails naming the skipped calls instead of grading 1.0. Without
// this safeguard an eval map that omits NoErrors would pass such a trace.
func OnlyToolCalls[T any](toolNames ...string) ObservableTraceCallback[T] {
	// Precompute the allowed set once when the callback is created
	allowed := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		allowed[name] = struct{}{}
	}

	return func(o Observer, trace *agenttrace.Trace[T]) {
		var skipped []string
		succeeded := false
		for _, tc := range trace.ToolCalls {
			if _, ok := allowed[tc.Name]; !ok {
				if errors.Is(tc.Error, agenttrace.ErrUnknownTool) {
					skipped = append(skipped, tc.Name)
					continue
				}
				o.Fail(fmt.Sprintf("unexpected tool call %q, only allowed: %v", tc.Name, toolNames))
				return
			}
			// The terminal submit commits the result rather than doing work,
			// so it cannot stand as the run's only success.
			if tc.Error == nil && !tc.Terminal {
				succeeded = true
			}
		}
		if n := len(skipped); n > 0 {
			if n > maxReportedToolErrors {
				skipped = append(skipped[:maxReportedToolErrors], fmt.Sprintf("and %d more", n-maxReportedToolErrors))
			}
			detail := strings.Join(skipped, "; ")
			if !succeeded {
				o.Fail(fmt.Sprintf("no tool call succeeded; executor rejected %d unknown-tool call(s): %s", n, detail))
				return
			}
			o.Grade(1.0, fmt.Sprintf("executor rejected %d unknown-tool call(s): %s", n, detail))
		}
	}
}

// RequiredToolCalls returns an ObservableTraceCallback that validates the trace uses all of the specified tool names at least once.
func RequiredToolCalls[T any](toolNames []string) ObservableTraceCallback[T] {
	// Precompute the required set once when the callback is created
	baseRequired := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		baseRequired[name] = struct{}{}
	}

	return func(o Observer, trace *agenttrace.Trace[T]) {
		// Copy the precomputed set for this invocation
		required := maps.Clone(baseRequired)

		// Mark off tools as we see them
		for _, tc := range trace.ToolCalls {
			delete(required, tc.Name)
		}

		// Check if any required tools were not used
		if len(required) > 0 {
			missing := make([]string, 0, len(required))
			for name := range required {
				missing = append(missing, name)
			}
			sort.Strings(missing)
			o.Fail(fmt.Sprintf("missing required tool calls: %v", missing))
		}
	}
}

// maxReportedToolErrors caps how many tool-call errors NoErrors and
// OnlyToolCalls list in their Grade reasoning, so the reasoning stays a
// readable size in score sinks (BigQuery rows, gate comments).
const maxReportedToolErrors = 3

// NoErrors returns an ObservableTraceCallback that validates the run reached a
// terminal result without an unrecovered error. It fails only when the trace
// itself carries an error (executor failure, turn budget exhausted, requeue).
//
// Tool-call errors do not fail the metric on their own. An errored call the
// agent worked past, by retrying, by taking another route, or by treating a
// not-found answer to a probe as the answer, is a recovered transient. This
// mirrors how agenttrace.RecordedTurn records transient errors without
// marking the turn Failed. Recovered tool-call errors reach score sinks
// through Grade with a reasoning string listing them, so the signal survives
// without gating on it.
//
// A completed run is not by itself proof the agent worked past those errors.
// The executors steer a stuck model to submit anyway, so a run whose every
// call failed can still commit a degraded result and leave Trace.Error nil.
// So a completed run that recorded tool-call errors and no successful tool
// call fails. The terminal submit (agenttrace.ToolCall.Terminal) is not a
// success for this count: committing a result is not work.
//
// An unknown-tool call, a call carrying agenttrace.ErrUnknownTool because it
// named a tool the executor has no handler for, is a protocol violation the
// executor rejects. On a completed run the agent worked past (no trace error,
// at least one successful non-terminal call), it joins the recovered
// transients: the run grades 1.0 with the error in the reasoning. It stays
// fatal under any trace error, including an infrastructure-marked one, and
// when no non-terminal call succeeded.
//
// Tool calls marked Recoverable stay out of that reasoning. They record a
// designed correction loop, where a handler rejected the call but returned a
// corrective hint the model acted on (e.g. a resubmitted terminal submit), so
// there is no tool misuse to report.
//
// Optional ignore functions can be provided to filter out expected errors
// (e.g., file not found). If any ignore function returns true for a given
// error, that error is skipped: it neither fails the metric nor appears in
// the Grade reasoning. This holds for an unknown-tool error too, so an agent
// that legitimately expects one can say so.
//
// A trace error that carries the workqueue.InfrastructureError marker is
// exempt: it grades 1.0 rather than failing. A provider outage (an unreachable
// model endpoint, a broken connection mid-call) is a failure of the
// infrastructure the run depends on, not an agent error, and scoring it as one
// turns an outage into a fleet-wide quality regression. The exemption keys on
// the marker (workqueue.HasInfrastructureMarker), not on
// workqueue.IsInfrastructureError, so only a failure the shared provider retry
// path marked is exempt: a bare codes.Unavailable the run produced itself still
// fails. The reasoning carries the error text and the marked causes, so the
// outage and the provider failure behind it still reach score sinks. An
// ordinary requeue is not exempt and still fails.
// The exemption never overrides an unknown-tool error: a trace error,
// infrastructure-marked or not, keeps a recorded unknown-tool call fatal.
//
// A suspended trace (Trace.Suspended) neither fails nor grades. Suspension is
// an intentional mid-run halt awaiting an out-of-band signal, not a failure,
// and the resumed run is graded on its own trace.
func NoErrors[T any](ignore ...func(error) bool) ObservableTraceCallback[T] {
	shouldIgnore := func(err error) bool {
		for _, fn := range ignore {
			if fn(err) {
				return true
			}
		}
		return false
	}
	// A call naming a tool the executor has no handler for. Matched through
	// the sentinel, never the message: the tool name in it is
	// model-controlled text.
	isUnknownTool := func(tc *agenttrace.ToolCall[T]) bool {
		return tc.Error != nil && !shouldIgnore(tc.Error) && errors.Is(tc.Error, agenttrace.ErrUnknownTool)
	}
	return func(o Observer, trace *agenttrace.Trace[T]) {
		// Short-circuit before the error checks: the suspension sentinel is
		// error-shaped and can surface through the trace's error channels
		// (e.g. recorded on the intercepted suspend tool call), and grading a
		// half-run's errors is premature. The resumed run completes on its
		// own trace.
		if trace.Suspended {
			return
		}

		// A trace-level error means the run died before reaching a result.
		if trace.Error != nil && !shouldIgnore(trace.Error) {
			// A provider outage is not an agent error, so grade it a pass and
			// keep the error text in the reasoning, where score sinks show it.
			// Matched through the marker, never the message: the text is
			// provider-controlled. The marker alone, so a bare codes.Unavailable
			// the run produced itself keeps failing.
			if workqueue.HasInfrastructureMarker(trace.Error) {
				// An outage does not make an unknown-tool call legal, so the
				// exemption never covers a trace that recorded one.
				for _, tc := range trace.ToolCalls {
					if isUnknownTool(tc) {
						o.Fail(fmt.Sprintf("tool call %s error: got = %v, wanted = nil", tc.Name, tc.Error))
						return
					}
				}
				reasoning := fmt.Sprintf("infrastructure error, not an agent error: %v", trace.Error)
				// The marker's message is the wrapped error's, which for a
				// requeue says only when the key comes back. The causes name
				// the provider failure, so append them or the outage signal
				// stops at the marker.
				if causes := workqueue.InfrastructureCauses(trace.Error); len(causes) > 0 {
					texts := make([]string, 0, len(causes))
					for _, cause := range causes {
						texts = append(texts, cause.Error())
					}
					reasoning += fmt.Sprintf(" (cause: %s)", strings.Join(texts, "; "))
				}
				o.Grade(1.0, reasoning)
				return
			}
			o.Fail(fmt.Sprintf("trace error: got = %v, wanted = nil", trace.Error))
			return
		}

		// Tool-call errors on a completed run are recovered transients as
		// long as some call succeeded: report those without failing.
		var recovered []string
		succeeded := 0
		for _, tc := range trace.ToolCalls {
			if tc.Error == nil || shouldIgnore(tc.Error) {
				// The terminal submit commits the result rather than doing
				// work, so it cannot stand as the run's only success.
				if !tc.Terminal {
					succeeded++
				}
				continue
			}
			if !tc.Recoverable {
				recovered = append(recovered, fmt.Sprintf("%s: %v", tc.Name, tc.Error))
			}
		}
		if n := len(recovered); n > 0 {
			if n > maxReportedToolErrors {
				recovered = append(recovered[:maxReportedToolErrors], fmt.Sprintf("and %d more", n-maxReportedToolErrors))
			}
			detail := strings.Join(recovered, "; ")
			if succeeded == 0 {
				o.Fail(fmt.Sprintf("no tool call succeeded; %d tool-call error(s): %s", n, detail))
				return
			}
			o.Grade(1.0, fmt.Sprintf("run completed; recovered from %d tool-call error(s): %s", n, detail))
		}
	}
}

// BuildCallbacks creates a list of TraceCallbacks from a namespaced observer and evaluation map.
// This helper injects each evaluation function with a child observer to create
// TraceCallbacks that can be used with ByCode or other tracers.
func BuildCallbacks[T any, O Observer](observer *NamespacedObserver[O], evalMap map[string]ObservableTraceCallback[T]) []agenttrace.TraceCallback[T] {
	callbacks := make([]agenttrace.TraceCallback[T], 0, len(evalMap))
	for name, evalFunc := range evalMap {
		callbacks = append(callbacks, Inject(observer.Child(name), evalFunc))
	}
	return callbacks
}

// BuildTracer creates a ByCode tracer from a namespaced observer and evaluation map.
// This helper consolidates the common pattern of setting up comprehensive evaluation
// tracers by injecting each evaluation function with a child observer and building
// a ByCode tracer from the resulting callbacks.
func BuildTracer[T any, O Observer](observer *NamespacedObserver[O], evalMap map[string]ObservableTraceCallback[T]) agenttrace.Tracer[T] {
	return agenttrace.ByCode(BuildCallbacks(observer, evalMap)...)
}

// ResultValidator returns an ObservableTraceCallback that validates the result using a custom validator.
// The validator is only called if the result is non-nil.
// T should typically be a pointer type like *MyStruct.
func ResultValidator[T any](validator func(result T) error) ObservableTraceCallback[T] {
	return func(o Observer, trace *agenttrace.Trace[T]) {
		// Use reflection to check if Result is a nil pointer
		v := reflect.ValueOf(trace.Result)
		if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
			o.Fail("result is nil")
			return
		}
		if err := validator(trace.Result); err != nil {
			o.Fail(err.Error())
		}
	}
}
