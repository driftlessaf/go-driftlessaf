/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

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

// NewTrace creates a new trace with the given prompt
func (t *byCodeTracer[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return newTraceWithTracer[T](ctx, t, prompt)
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
