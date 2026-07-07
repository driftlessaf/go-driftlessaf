/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"sync"
)

// CaptureTrace returns a derived context whose Tracer[T] tees every completed
// trace into an accumulator, plus a function returning the most recently
// completed trace. The previously-installed tracer (CloudEvent emission,
// logging, ...) is forwarded unchanged — this only adds a side effect, it
// never replaces or skips the original behavior.
//
// When several traces of the same T complete under the returned context, the
// most recent wins: for the meta-reconcilers' use (one agent execution per
// capture scope) that is the agent run itself, and a retried run cleanly
// supersedes its predecessor. The accessor reports nil until a trace
// completes, so downstream rendering (SummarizeTraceReasoning returns "" for
// nil) degrades to exactly the pre-capture behavior.
//
// Each call installs a fresh accumulator, so concurrent reconciliations
// (each with their own derived context) never share state.
func CaptureTrace[T any](ctx context.Context) (context.Context, func() *Trace[T]) {
	c := &traceCapture[T]{inner: TracerFromContext[T](ctx)}
	return WithTracer[T](ctx, c), c.last
}

// traceCapture is the teeing Tracer[T] installed by CaptureTrace.
type traceCapture[T any] struct {
	inner Tracer[T]

	mu       sync.Mutex
	captured *Trace[T]
}

func (c *traceCapture[T]) NewTrace(ctx context.Context, prompt string, opts ...StartTraceOption) *Trace[T] {
	return c.inner.NewTrace(ctx, prompt, opts...)
}

func (c *traceCapture[T]) RecordTrace(trace *Trace[T]) {
	c.inner.RecordTrace(trace)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = trace
}

func (c *traceCapture[T]) last() *Trace[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}
