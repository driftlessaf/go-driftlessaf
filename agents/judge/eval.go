/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"chainguard.dev/driftlessaf/agents/evals"
)

// NewGoldenEval creates an evaluation function for golden mode judgment
func NewGoldenEval[T any](j Interface, criterion string, goldenAnswer string, callbacks ...evals.TraceCallback[*Judgement]) evals.ObservableTraceCallback[T] {
	return func(o evals.Observer, trace *evals.Trace[T]) {
		// Extract actual response from trace.Result
		// Use reflection-based nil check that works with generic types
		if isNilResult(trace.Result) {
			o.Fail("Failed to extract response: trace has no result")
			return
		}

		// JSON encode with indentation for readability
		data, err := json.MarshalIndent(trace.Result, "", "  ")
		if err != nil {
			o.Fail(fmt.Sprintf("Failed to extract response: failed to marshal result: %v", err))
			return
		}

		// Get judgment with ByCode tracer injected (allows caller to specify evals for the judge itself, but alternate purpose is to quiet default logging during tests)
		// Start with background context but preserve ExecutionContext from trace for metrics labeling
		ctx := evals.WithTracer(context.Background(), evals.ByCode(callbacks...))
		ctx = evals.WithExecutionContext(ctx, trace.ExecContext)
		resp, err := j.Judge(ctx, &Request{
			Mode:            GoldenMode,
			ReferenceAnswer: goldenAnswer,
			ActualAnswer:    string(data),
			Criterion:       criterion,
		})
		if err != nil {
			o.Fail(fmt.Sprintf("Judge failed: %v", err))
			return
		}
		if resp == nil {
			o.Fail("Judge returned nil response")
			return
		}

		// Grade the judgment with score and reasoning
		o.Grade(resp.Score, resp.Reasoning)

		// Log suggestions if available
		if len(resp.Suggestions) > 0 {
			for _, suggestion := range resp.Suggestions {
				o.Log(fmt.Sprintf("  Suggestion: %s", suggestion))
			}
		}
	}
}

// NewStandaloneEval creates an evaluation function for standalone mode judgment
func NewStandaloneEval[T any](j Interface, criterion string, callbacks ...evals.TraceCallback[*Judgement]) evals.ObservableTraceCallback[T] {
	return func(o evals.Observer, trace *evals.Trace[T]) {
		// Extract actual response from trace.Result
		// Use reflection-based nil check that works with generic types
		if isNilResult(trace.Result) {
			o.Fail("Failed to extract response: trace has no result")
			return
		}

		// JSON encode with indentation for readability
		data, err := json.MarshalIndent(trace.Result, "", "  ")
		if err != nil {
			o.Fail(fmt.Sprintf("Failed to extract response: failed to marshal result: %v", err))
			return
		}

		// Get judgment with ByCode tracer injected (allows caller to specify evals for the judge itself, but alternate purpose is to quiet default logging during tests)
		// Start with background context but preserve ExecutionContext from trace for metrics labeling
		ctx := evals.WithTracer(context.Background(), evals.ByCode(callbacks...))
		ctx = evals.WithExecutionContext(ctx, trace.ExecContext)
		resp, err := j.Judge(ctx, &Request{
			Mode:         StandaloneMode,
			ActualAnswer: string(data),
			Criterion:    criterion,
		})
		if err != nil {
			o.Fail(fmt.Sprintf("Judge failed: %v", err))
			return
		}
		if resp == nil {
			o.Fail("Judge returned nil response")
			return
		}

		// Grade the judgment with score and reasoning
		o.Grade(resp.Score, resp.Reasoning)

		// Log suggestions if available
		if len(resp.Suggestions) > 0 {
			for _, suggestion := range resp.Suggestions {
				o.Log(fmt.Sprintf("  Suggestion: %s", suggestion))
			}
		}
	}
}

// isNilResult checks if the generic value is nil using reflection
func isNilResult[T any](value T) bool {
	v := reflect.ValueOf(value)
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
