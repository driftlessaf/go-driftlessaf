/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"fmt"
	"maps"
	"reflect"
	"sort"
)

// ExactToolCalls returns an ObservableTraceCallback that validates the trace has exactly n tool calls.
func ExactToolCalls[T any](n int) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		if got := len(trace.ToolCalls); got != n {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted = %d", got, n))
		}
	}
}

// MinimumNToolCalls returns an ObservableTraceCallback that validates the trace has at least n tool calls.
func MinimumNToolCalls[T any](n int) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		if got := len(trace.ToolCalls); got < n {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted >= %d", got, n))
		}
	}
}

// MaximumNToolCalls returns an ObservableTraceCallback that validates the trace has at most n tool calls.
func MaximumNToolCalls[T any](n int) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		if got := len(trace.ToolCalls); got > n {
			o.Fail(fmt.Sprintf("tool call count: got = %d, wanted <= %d", got, n))
		}
	}
}

// RangeToolCalls returns an ObservableTraceCallback that validates the trace has between min and max tool calls (inclusive).
func RangeToolCalls[T any](min, max int) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
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
func OnlyToolCalls[T any](toolNames ...string) ObservableTraceCallback[T] {
	// Precompute the allowed set once when the callback is created
	allowed := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		allowed[name] = struct{}{}
	}

	return func(o Observer, trace *Trace[T]) {
		for _, tc := range trace.ToolCalls {
			if _, ok := allowed[tc.Name]; !ok {
				o.Fail(fmt.Sprintf("unexpected tool call %q, only allowed: %v", tc.Name, toolNames))
				return
			}
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

	return func(o Observer, trace *Trace[T]) {
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

// ToolCallValidator creates an ObservableTraceCallback that validates individual tool calls using a custom validator function.
func ToolCallValidator[T any](validator func(o Observer, tc *ToolCall[T]) error) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		for i, tc := range trace.ToolCalls {
			if err := validator(o, tc); err != nil {
				o.Fail(fmt.Sprintf("tool call %d (%s) validation failed: %v", i, tc.Name, err))
				return
			}
		}
	}
}

// ToolCallNamed returns an ObservableTraceCallback that validates tool calls with a specific name using a custom validator.
func ToolCallNamed[T any](name string, validator func(o Observer, tc *ToolCall[T]) error) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		found := false
		for _, tc := range trace.ToolCalls {
			if tc.Name == name {
				found = true
				if err := validator(o, tc); err != nil {
					o.Fail(fmt.Sprintf("tool call %s validation failed: %v", name, err))
					return
				}
			}
		}

		if !found {
			o.Fail(fmt.Sprintf("tool call named %q: got = not found, wanted = found", name))
		}
	}
}

// NoErrors returns an ObservableTraceCallback that validates no tool calls resulted in errors.
func NoErrors[T any]() ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		// Check trace error
		if trace.Error != nil {
			o.Fail(fmt.Sprintf("trace error: got = %v, wanted = nil", trace.Error))
			return
		}

		// Check tool call errors
		for _, tc := range trace.ToolCalls {
			if tc.Error != nil {
				o.Fail(fmt.Sprintf("tool call %s error: got = %v, wanted = nil", tc.Name, tc.Error))
				return
			}
		}
	}
}

// BuildCallbacks creates a list of TraceCallbacks from a namespaced observer and evaluation map.
// This helper injects each evaluation function with a child observer to create
// TraceCallbacks that can be used with ByCode or other tracers.
func BuildCallbacks[T any, O Observer](observer *NamespacedObserver[O], evalMap map[string]ObservableTraceCallback[T]) []TraceCallback[T] {
	callbacks := make([]TraceCallback[T], 0, len(evalMap))
	for name, evalFunc := range evalMap {
		callbacks = append(callbacks, Inject(observer.Child(name), evalFunc))
	}
	return callbacks
}

// BuildTracer creates a ByCode tracer from a namespaced observer and evaluation map.
// This helper consolidates the common pattern of setting up comprehensive evaluation
// tracers by injecting each evaluation function with a child observer and building
// a ByCode tracer from the resulting callbacks.
func BuildTracer[T any, O Observer](observer *NamespacedObserver[O], evalMap map[string]ObservableTraceCallback[T]) Tracer[T] {
	return ByCode(BuildCallbacks(observer, evalMap)...)
}

// ResultValidator returns an ObservableTraceCallback that validates the result using a custom validator.
// The validator is only called if the result is non-nil.
// T should typically be a pointer type like *MyStruct.
func ResultValidator[T any](validator func(result T) error) ObservableTraceCallback[T] {
	return func(o Observer, trace *Trace[T]) {
		// Use reflection to check if Result is a nil pointer
		v := reflect.ValueOf(trace.Result)
		if !v.IsValid() || (v.Kind() == reflect.Ptr && v.IsNil()) {
			o.Fail("result is nil")
			return
		}
		if err := validator(trace.Result); err != nil {
			o.Fail(err.Error())
		}
	}
}
