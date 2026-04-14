/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// TraceCallback is a function that receives completed traces
type TraceCallback[T any] func(*Trace[T])

// byCodeTracer implements Tracer by invoking callback functions for code-based evals
type byCodeTracer[T any] struct {
	callbacks []TraceCallback[T]
}

// ByCode creates a new Tracer for code-based evals that invokes the given callbacks when traces are recorded
func ByCode[T any](callbacks ...TraceCallback[T]) Tracer[T] {
	return &byCodeTracer[T]{
		callbacks: callbacks,
	}
}

// NewTrace creates a new trace with the given prompt.
//
// This deliberately does NOT pass the byCodeTracer itself into newTrace.
// The trace stores ctx, and Complete() later calls TracerFromContext(ctx) to
// find the tracer to invoke RecordTrace on.
//
// This indirection is what makes decorator composition work. The call path is:
//
//  1. Middleware installs decorators via WithTracer(ctx, outerDecorator).
//  2. StartTrace(ctx, prompt) finds outerDecorator on ctx, calls its NewTrace.
//  3. Each decorator delegates NewTrace inward (t.wrapped.NewTrace(ctx, ...)),
//     passing the same ctx through. Crucially, no decorator replaces the tracer
//     on ctx during NewTrace — the outermost decorator remains on ctx.
//  4. The leaf (byCodeTracer) reaches here and calls newTrace(ctx, prompt).
//     The trace stores ctx as-is.
//  5. trace.Complete() calls TracerFromContext(storedCtx), which resolves to
//     the outermost decorator — so the entire decorator chain's RecordTrace
//     methods run (outer → inner), not just the leaf's.
//
// Without this, storing a back-pointer to the leaf tracer would silently
// bypass any decorator that hooks RecordTrace (e.g. a CloudEvents emitter).
func (t *byCodeTracer[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return newTrace[T](ctx, prompt)
}

// RecordTrace invokes all callbacks with the completed trace in parallel
func (t *byCodeTracer[T]) RecordTrace(trace *Trace[T]) {
	// Use errgroup to run callbacks in parallel
	g := new(errgroup.Group)

	for _, callback := range t.callbacks {
		if callback != nil {
			g.Go(func() error {
				callback(trace)
				return nil
			})
		}
	}

	// Wait for all callbacks to complete
	// We ignore the error since our callbacks always return nil
	_ = g.Wait()
}
