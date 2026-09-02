/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chainguard-dev/clog"
)

// NewDefaultTracer creates a default tracer that logs to clog. The log record
// always contains the structural trace. When payloads are enabled on the trace
// context, the trace also contains its payload fields.
//
// We use trace.ctx (not the startup ctx) so that each log line carries the
// per-request context — including trace metadata, reconciler key, etc.
//
// Use NewMetadataTracer when the log sink must never receive raw payloads,
// including when a caller enables them for another sink.
func NewDefaultTracer[T any](_ context.Context) Tracer[T] {
	return ByCode[T](func(trace *Trace[T]) {
		traceValue := any(trace)
		if !payloadsEnabledFrom(trace.ctx) {
			projected, err := projectTraceForLog(trace)
			if err != nil {
				clog.WarnContext(trace.ctx, "Agent trace projection failed",
					"trace_id", trace.ID,
				)

				logTraceMetadata(trace)
				return
			}

			traceValue = projected
		}

		clog.InfoContext(trace.ctx, "Agent trace completed",
			"trace_id", trace.ID,
			"agent_name", trace.AgentName,
			"model", trace.Model,
			"duration_ms", trace.Duration().Milliseconds(),
			"tool_calls", len(trace.ToolCalls),
			"failed", trace.Error != nil,
			"trace", traceValue,
		)
	})
}

func projectTraceForLog[T any](trace *Trace[T]) (json.RawMessage, error) {
	raw, err := json.Marshal(trace)
	if err != nil {
		return nil, fmt.Errorf("marshal trace: %w", err)
	}

	projected, err := omitSensitiveTraceFields(raw)
	if err != nil {
		return nil, fmt.Errorf("omit trace payloads: %w", err)
	}

	return json.RawMessage(projected), nil
}

// NewMetadataTracer creates a tracer that always logs trace metadata only: id,
// agent, model, duration, tool-call count, and error state. It does not log the
// trace document, even when payloads are enabled on the trace context.
func NewMetadataTracer[T any](_ context.Context) Tracer[T] {
	return ByCode[T](logTraceMetadata[T])
}

func logTraceMetadata[T any](trace *Trace[T]) {
	clog.InfoContext(trace.ctx, "Agent trace completed",
		"trace_id", trace.ID,
		"agent_name", trace.AgentName,
		"model", trace.Model,
		"duration_ms", trace.Duration().Milliseconds(),
		"tool_calls", len(trace.ToolCalls),
		"failed", trace.Error != nil,
	)
}
