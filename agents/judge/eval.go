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

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
	"github.com/chainguard-dev/clog"
)

// judgeAttempts is the number of times to call the judge before giving up.
// A judge transport or JSON-parse error is transient (Gemini occasionally
// emits structured output with unescaped control characters), so a fresh
// request almost always parses. The first attempt plus two retries gives
// three total tries.
const judgeAttempts = 3

// judgeWithRetry calls j.Judge up to judgeAttempts times, returning the first
// successful judgement. Each error is transient (transport or parse), so a
// fresh request usually succeeds. It returns the last error if every attempt
// fails. The caller decides whether to skip rather than fail the metric, so a
// judge outage does not masquerade as a genuine low grade.
func judgeWithRetry(ctx context.Context, j Interface, req *Request, criterion string) (*Judgement, error) {
	var resp *Judgement
	var err error
	for attempt := range judgeAttempts {
		resp, err = j.Judge(ctx, req)
		if err == nil {
			return resp, nil
		}
		clog.WarnContext(ctx, "judge eval attempt failed",
			"criterion", criterion,
			"attempt", attempt+1,
			"attempts", judgeAttempts,
			"error", err)
	}
	return nil, err
}

// NewGoldenEval creates an evaluation function for golden mode judgment
func NewGoldenEval[T any](j Interface, criterion string, goldenAnswer string, callbacks ...agenttrace.TraceCallback[*Judgement]) evals.ObservableTraceCallback[T] {
	return func(o evals.Observer, trace *agenttrace.Trace[T]) {
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

		// Derive from the trace's own ctx so the judge inherits the reconciler's
		// WithDefaultNameFn ("autofix: pr:...", "skillup: ...", "manifest-gen: ..."),
		// WithDefaultAgentName, WithPayloadsEnabled, and the active OTel span
		// parent. Without this, every judge-emitted invoke_agent span surfaces as
		// an orphan root named "judge" with no link to the parent trace tree; any
		// outbound HTTP call inside the judge similarly becomes an orphan root
		// (e.g. "HTTP POST" from otelhttp instrumentation) because it has no
		// active OTel span to parent under.
		//
		// context.WithoutCancel detaches the cancellation chain: callbacks fire
		// after Complete(), so the original request's ctx may already be Done.
		// We want to inherit the values (Default*, ExecContext, the OTel span
		// for parentage) without inheriting the deadline. Available since Go 1.21.
		//
		// Fall back to Background() when trace.Context() is nil: tests construct
		// Trace[T] as a struct literal (bypassing newTrace) and never set ctx.
		// Production code always goes through newTrace, which seeds ctx.
		//
		// WithTracer overrides only the in-process tracer chain so judge evals
		// don't recurse back through the parent tracer's callbacks.
		parentCtx := trace.Context()
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		ctx := context.WithoutCancel(parentCtx)
		ctx = agenttrace.WithTracer(ctx, agenttrace.ByCode(callbacks...))
		resp, err := judgeWithRetry(ctx, j, &Request{
			Mode:            GoldenMode,
			ReferenceAnswer: goldenAnswer,
			ActualAnswer:    string(data),
			Criterion:       criterion,
		}, criterion)
		if err != nil {
			// A judge transport/parse error is not a quality signal. After
			// exhausting retries, skip the metric (no Fail, no Grade) rather
			// than record a pass-rate-0 failure that would fail the test on a
			// judge-infra blip. The warning keeps the outage visible in the
			// job log, which the observer's Fail text does not surface.
			clog.WarnContext(ctx, "judge eval skipped after exhausting retries", "criterion", criterion, "error", err)
			return
		}
		if resp == nil {
			clog.WarnContext(ctx, "judge eval skipped: nil response", "criterion", criterion)
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
func NewStandaloneEval[T any](j Interface, criterion string, callbacks ...agenttrace.TraceCallback[*Judgement]) evals.ObservableTraceCallback[T] {
	return func(o evals.Observer, trace *agenttrace.Trace[T]) {
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

		// Derive from the trace's own ctx so the judge inherits the reconciler's
		// WithDefaultNameFn ("autofix: pr:...", "skillup: ...", "manifest-gen: ..."),
		// WithDefaultAgentName, WithPayloadsEnabled, and the active OTel span
		// parent. Without this, every judge-emitted invoke_agent span surfaces as
		// an orphan root named "judge" with no link to the parent trace tree; any
		// outbound HTTP call inside the judge similarly becomes an orphan root
		// (e.g. "HTTP POST" from otelhttp instrumentation) because it has no
		// active OTel span to parent under.
		//
		// context.WithoutCancel detaches the cancellation chain: callbacks fire
		// after Complete(), so the original request's ctx may already be Done.
		// We want to inherit the values (Default*, ExecContext, the OTel span
		// for parentage) without inheriting the deadline. Available since Go 1.21.
		//
		// Fall back to Background() when trace.Context() is nil: tests construct
		// Trace[T] as a struct literal (bypassing newTrace) and never set ctx.
		// Production code always goes through newTrace, which seeds ctx.
		//
		// WithTracer overrides only the in-process tracer chain so judge evals
		// don't recurse back through the parent tracer's callbacks.
		parentCtx := trace.Context()
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		ctx := context.WithoutCancel(parentCtx)
		ctx = agenttrace.WithTracer(ctx, agenttrace.ByCode(callbacks...))
		resp, err := judgeWithRetry(ctx, j, &Request{
			Mode:         StandaloneMode,
			ActualAnswer: string(data),
			Criterion:    criterion,
		}, criterion)
		if err != nil {
			// See NewGoldenEval: a judge transport/parse error is not a quality
			// signal, so skip rather than fail after retries are exhausted. The
			// warning keeps the outage visible in the job log.
			clog.WarnContext(ctx, "judge eval skipped after exhausting retries", "criterion", criterion, "error", err)
			return
		}
		if resp == nil {
			clog.WarnContext(ctx, "judge eval skipped: nil response", "criterion", criterion)
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
