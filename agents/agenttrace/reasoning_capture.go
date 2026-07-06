/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"sync"
)

// CaptureReasoning returns a derived context whose Tracer[T] tees every
// completed trace's extended-thinking blocks into an accumulator, plus a
// function returning the blocks captured so far. The previously-installed
// tracer (CloudEvent emission, logging, ...) is forwarded unchanged — this
// only adds a side effect, it never replaces or skips the original behavior.
//
// When several traces of the same T complete under the returned context, the
// blocks of the most recent trace that carried any reasoning win: for the
// meta-reconcilers' use (one agent execution per capture scope) that is the
// agent run itself, and a retried run cleanly supersedes its predecessor.
// The returned accessor reports nil until a trace with reasoning completes —
// e.g. when extended thinking is disabled for the agent, or a run produced
// none — so downstream rendering (SummarizeReasoning returns "" for empty
// input) degrades to exactly the pre-capture behavior.
//
// Each call installs a fresh accumulator, so concurrent reconciliations
// (each with their own derived context) never share state.
func CaptureReasoning[T any](ctx context.Context) (context.Context, func() []ReasoningContent) {
	c := &reasoningCapture[T]{inner: TracerFromContext[T](ctx)}
	return WithTracer[T](ctx, c), c.blocks
}

// reasoningCapture is the teeing Tracer[T] installed by CaptureReasoning.
type reasoningCapture[T any] struct {
	inner Tracer[T]

	mu       sync.Mutex
	captured []ReasoningContent
}

func (c *reasoningCapture[T]) NewTrace(ctx context.Context, prompt string, opts ...StartTraceOption) *Trace[T] {
	return c.inner.NewTrace(ctx, prompt, opts...)
}

func (c *reasoningCapture[T]) RecordTrace(trace *Trace[T]) {
	c.inner.RecordTrace(trace)
	if len(trace.Reasoning) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = trace.Reasoning
}

func (c *reasoningCapture[T]) blocks() []ReasoningContent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}
