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
	// Create a callback that logs traces
	callback := func(trace *Trace[T]) {
		// Log the structured trace representation
		clog.InfoContext(ctx, "Agent trace completed",
			"trace_id", trace.ID,
			"duration_ms", trace.Duration().Milliseconds(),
			"tool_calls", len(trace.ToolCalls),
			"trace", trace.String(),
		)
	}

	return ByCode[T](callback)
}
