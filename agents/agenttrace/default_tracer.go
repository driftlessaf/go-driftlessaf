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
//
// The logged document embeds the trace's payloads (input_prompt, result,
// tool-call contents). Services whose agents run on confidential input must
// install NewMetadataTracer instead, so those payloads never reach log sinks.
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

// NewMetadataTracer creates a tracer that logs trace METADATA only — id,
// agent, model, duration, tool-call count, error state — never the trace
// document itself (input_prompt, result, tool-call payloads). Install it
// (per result type, via WithTracer) in services whose agents run on
// confidential input that must not reach log sinks; the default tracer's
// full-trace log line is exactly the leak this prevents.
func NewMetadataTracer[T any](_ context.Context) Tracer[T] {
	return ByCode[T](func(trace *Trace[T]) {
		clog.InfoContext(trace.ctx, "Agent trace completed",
			"trace_id", trace.ID,
			"agent_name", trace.AgentName,
			"model", trace.Model,
			"duration_ms", trace.Duration().Milliseconds(),
			"tool_calls", len(trace.ToolCalls),
			"failed", trace.Error != nil,
		)
	})
}
