/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"context"
)

// tracerKey is the context key for storing values of type T
type tracerKey[T any] struct{}

// Tracer is the interface for creating and managing traces
type Tracer[T any] interface {
	// NewTrace creates a new trace with the given prompt
	NewTrace(ctx context.Context, prompt string) *Trace[T]
	// RecordTrace records a completed trace
	RecordTrace(trace *Trace[T])
}

// WithTracer returns a new context with the given tracer
func WithTracer[T any](ctx context.Context, tracer Tracer[T]) context.Context {
	return context.WithValue(ctx, tracerKey[T]{}, tracer)
}

// TracerFromContext returns the tracer from the context, or creates a default tracer
func TracerFromContext[T any](ctx context.Context) Tracer[T] {
	if tracer, ok := ctx.Value(tracerKey[T]{}).(Tracer[T]); ok {
		return tracer
	}
	return NewDefaultTracer[T](ctx)
}

// StartTrace starts a new trace using the tracer from the context
func StartTrace[T any](ctx context.Context, prompt string) *Trace[T] {
	tracer := TracerFromContext[T](ctx)
	return tracer.NewTrace(ctx, prompt)
}
