/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ReasoningContent represents internal reasoning from an LLM
type ReasoningContent struct {
	Thinking string `json:"thinking"`
}

// ToolCall represents a single tool invocation within a trace
type ToolCall[T any] struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Params    map[string]any `json:"params"`
	Result    any            `json:"result"`
	Error     error          `json:"error,omitempty"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	trace     *Trace[T]      // Parent trace for auto-adding on completion
	mu        sync.Mutex     // Protects mutable fields
	ctx       context.Context
	span      oteltrace.Span
}

// Trace represents a complete agent interaction from prompt to result
type Trace[T any] struct {
	ID          string             `json:"id"`
	InputPrompt string             `json:"input_prompt"`
	ExecContext ExecutionContext   `json:"exec_context,omitempty"` // PR/commit metadata
	ToolCalls   []*ToolCall[T]     `json:"tool_calls"`
	Reasoning   []ReasoningContent `json:"reasoning,omitempty"`
	Result      T                  `json:"result"`
	Error       error              `json:"error,omitempty"`
	StartTime   time.Time          `json:"start_time"`
	EndTime     time.Time          `json:"end_time"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
	tracer      Tracer[T]          // Tracer for auto-recording
	mu          sync.Mutex         // Protects mutable fields
	ctx         context.Context
	span        oteltrace.Span
}

// newTraceWithTracer creates a new trace with the given tracer and prompt
func newTraceWithTracer[T any](ctx context.Context, tracer Tracer[T], prompt string) *Trace[T] {
	// Extract execution context from Go context
	execCtx := GetExecutionContext(ctx)

	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))

	// Add execution context as span attributes
	spanAttrs := []oteltrace.SpanStartOption{
		oteltrace.WithAttributes(attribute.String("agent.prompt", prompt)),
	}
	if execCtx.ReconcilerKey != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("reconciler_key", execCtx.ReconcilerKey)))
	}
	if execCtx.ReconcilerType != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("reconciler_type", execCtx.ReconcilerType)))
	}
	if execCtx.CommitSHA != "" {
		spanAttrs = append(spanAttrs, oteltrace.WithAttributes(attribute.String("commit_sha", execCtx.CommitSHA)))
	}

	ctx, span := tr.Start(ctx, "agent.execution", spanAttrs...)

	return &Trace[T]{
		ID:          generateTraceID(),
		InputPrompt: prompt,
		ExecContext: execCtx,
		ToolCalls:   []*ToolCall[T]{},
		StartTime:   time.Now(),
		Metadata:    make(map[string]any),
		tracer:      tracer,
		ctx:         ctx,
		span:        span,
	}
}

// StartToolCall starts a new tool call and returns it
func (t *Trace[T]) StartToolCall(id, name string, params map[string]any) *ToolCall[T] {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))
	ctx, span := tr.Start(t.ctx, "agent.tool_call", oteltrace.WithAttributes(
		attribute.String("tool.name", name),
		attribute.String("tool.id", id),
	))

	return &ToolCall[T]{
		ID:        id,
		Name:      name,
		Params:    params,
		StartTime: time.Now(),
		trace:     t,
		ctx:       ctx,
		span:      span,
	}
}

// RecordTokenUsage records model and token usage as span attributes for observability.
// This allows viewing token consumption directly in Cloud Trace without needing to
// cross-reference with metrics.
func (t *Trace[T]) RecordTokenUsage(model string, inputTokens, outputTokens int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.span != nil {
		t.span.SetAttributes(
			attribute.String("model", model),
			attribute.Int64("tokens.input", inputTokens),
			attribute.Int64("tokens.output", outputTokens),
			attribute.Int64("tokens.total", inputTokens+outputTokens),
		)
	}
}

// BadToolCall records a tool call that failed due to bad arguments or unknown tool
func (t *Trace[T]) BadToolCall(id, name string, params map[string]any, err error) {
	tr := otel.Tracer("chainguard.ai.agents.agenttrace",
		oteltrace.WithInstrumentationVersion("1.0.0"))
	_, span := tr.Start(t.ctx, "agent.tool_call", oteltrace.WithAttributes(
		attribute.String("tool.name", name),
		attribute.String("tool.id", id),
		attribute.String("error", err.Error()),
	))
	span.SetStatus(codes.Error, err.Error())
	span.End()

	tc := &ToolCall[T]{
		ID:        id,
		Name:      name,
		Params:    params,
		StartTime: time.Now(),
		EndTime:   time.Now(),
		Error:     err,
		trace:     t,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.ToolCalls = append(t.ToolCalls, tc)
}

// Complete marks the tool call as complete and adds it to the parent trace
func (tc *ToolCall[T]) Complete(result any, err error) {
	tc.mu.Lock()
	tc.Result = result
	tc.Error = err
	tc.EndTime = time.Now()
	trace := tc.trace
	span := tc.span
	tc.mu.Unlock()

	if span != nil {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}

	// Auto-add to parent trace
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.ToolCalls = append(trace.ToolCalls, tc)
}

// Duration returns the duration of the tool call
func (tc *ToolCall[T]) Duration() time.Duration {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.EndTime.IsZero() {
		return time.Since(tc.StartTime)
	}
	return tc.EndTime.Sub(tc.StartTime)
}

// Complete marks the trace as complete with the given result and automatically records it
func (t *Trace[T]) Complete(result T, err error) {
	t.mu.Lock()
	t.Result = result
	t.Error = err
	t.EndTime = time.Now()
	tracer := t.tracer
	span := t.span
	t.mu.Unlock()

	if span != nil {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}

	// Auto-record with tracer
	tracer.RecordTrace(t)
}

// Duration returns the total duration of the trace
func (t *Trace[T]) Duration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.EndTime.IsZero() {
		return time.Since(t.StartTime)
	}
	return t.EndTime.Sub(t.StartTime)
}

// String returns a structured representation of the trace
func (t *Trace[T]) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var sb strings.Builder

	// Calculate duration while we have the lock
	var duration time.Duration
	if t.EndTime.IsZero() {
		duration = time.Since(t.StartTime)
	} else {
		duration = t.EndTime.Sub(t.StartTime)
	}

	// Header
	sb.WriteString(fmt.Sprintf("=== Trace %s ===\n", t.ID))
	sb.WriteString(fmt.Sprintf("Prompt: %q\n", t.InputPrompt))
	sb.WriteString(fmt.Sprintf("Duration: %v\n", duration))

	// Reasoning
	if len(t.Reasoning) > 0 {
		sb.WriteString(fmt.Sprintf("\nReasoning (%d blocks):\n", len(t.Reasoning)))
		for i, r := range t.Reasoning {
			thinkingStr := r.Thinking
			if len(thinkingStr) > 200 {
				thinkingStr = thinkingStr[:197] + "..."
			}
			sb.WriteString(fmt.Sprintf("  [%d] %s\n", i+1, thinkingStr))
		}
	}

	// Tool calls
	if len(t.ToolCalls) > 0 {
		sb.WriteString(fmt.Sprintf("\nTool Calls (%d):\n", len(t.ToolCalls)))
		for i, tc := range t.ToolCalls {
			sb.WriteString(fmt.Sprintf("  [%d] %s (ID: %s)\n", i+1, tc.Name, tc.ID))

			// Calculate tool call duration inline to avoid nested mutex lock
			var tcDuration time.Duration
			if tc.EndTime.IsZero() {
				tcDuration = time.Since(tc.StartTime)
			} else {
				tcDuration = tc.EndTime.Sub(tc.StartTime)
			}
			sb.WriteString(fmt.Sprintf("      Duration: %v\n", tcDuration))

			// Parameters
			if len(tc.Params) > 0 {
				sb.WriteString("      Params:\n")
				for k, v := range tc.Params {
					sb.WriteString(fmt.Sprintf("        %s: %v\n", k, v))
				}
			}

			// Result/Error
			if tc.Error != nil {
				sb.WriteString(fmt.Sprintf("      Error: %v\n", tc.Error))
			} else if tc.Result != nil {
				// Limit result output to avoid huge logs
				resultStr := fmt.Sprintf("%v", tc.Result)
				if len(resultStr) > 200 {
					resultStr = resultStr[:197] + "..."
				}
				sb.WriteString(fmt.Sprintf("      Result: %s\n", resultStr))
			}
		}
	} else {
		sb.WriteString("\nNo tool calls\n")
	}

	// Final result/error
	sb.WriteString("\nCompletion:\n")
	switch {
	case t.Error != nil:
		sb.WriteString(fmt.Sprintf("  Error: %v\n", t.Error))
	case any(t.Result) != nil:
		// Limit result output
		resultStr := fmt.Sprintf("%v", t.Result)
		if len(resultStr) > 500 {
			resultStr = resultStr[:497] + "..."
		}
		sb.WriteString(fmt.Sprintf("  Result: %s\n", resultStr))
	default:
		sb.WriteString("  Result: <nil>\n")
	}

	// Metadata if present
	if len(t.Metadata) > 0 {
		sb.WriteString("\nMetadata:\n")
		for k, v := range t.Metadata {
			sb.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
		}
	}

	return sb.String()
}

// generateTraceID generates a unique trace ID
func generateTraceID() string {
	// Generate a random component
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp only if random generation fails
		return time.Now().Format("20060102-150405.000000")
	}
	// Format: YYYYMMDD-HHMMSS-RRRR where RRRR is random hex
	return fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(b))
}
