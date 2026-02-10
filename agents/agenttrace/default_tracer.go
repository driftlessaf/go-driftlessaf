/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"

	"github.com/chainguard-dev/clog"
)

// NewDefaultTracer creates a new default tracer that logs to clog
func NewDefaultTracer[T any](ctx context.Context) Tracer[T] {
	logger := clog.FromContext(ctx)

	// Create a callback that logs traces
	callback := func(trace *Trace[T]) {
		// Log the structured trace representation
		logger.With(
			"trace_id", trace.ID,
			"duration_ms", trace.Duration().Milliseconds(),
			"tool_calls", len(trace.ToolCalls),
		).Info("Agent trace completed", "trace", trace.String())
	}

	return ByCode[T](callback)
}
