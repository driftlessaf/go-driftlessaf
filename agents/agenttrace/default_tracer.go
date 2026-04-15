/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"

	"github.com/chainguard-dev/clog"
)

// NewDefaultTracer creates a new default tracer that logs to clog.
// The trace is logged as a structured JSON document via MarshalJSON so
// that JSON log sinks (Cloud Logging, etc.) receive a parseable record.
//
// We use trace.ctx (not the startup ctx) so that each log line carries the
// per-request context — including trace metadata, reconciler key, etc.
func NewDefaultTracer[T any](_ context.Context) Tracer[T] {
	return ByCode[T](func(trace *Trace[T]) {
		clog.InfoContext(trace.ctx, "Agent trace completed",
			"trace_id", trace.ID,
			"duration_ms", trace.Duration().Milliseconds(),
			"tool_calls", len(trace.ToolCalls),
			"trace", trace,
		)
	})
}
