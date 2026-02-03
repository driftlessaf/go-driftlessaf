/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

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

// ExecutionContext provides reconciler-level context for agent executions.
// This context is used to enrich metrics with labels for tracking token usage
// and tool calls per reconciler (PR, path, etc.).
type ExecutionContext struct {
	ReconcilerKey  string `json:"reconciler_key,omitempty"`  // Primary identifier: "pr:chainguard-dev/enterprise-packages/41025" or "path:chainguard-dev/mono/main/images/nginx"
	ReconcilerType string `json:"reconciler_type,omitempty"` // Type of reconciler: "pr" or "path"
	CommitSHA      string `json:"commit_sha,omitempty"`      // Git commit SHA (optional, for git-based reconcilers)
	TurnNumber     int    `json:"turn_number,omitempty"`     // Turn number for multi-turn agents (optional, 1, 2, 3, ...)
}

// Repository extracts the repository from the reconciler key.
// For "pr:chainguard-dev/enterprise-packages/41025" returns "chainguard-dev/enterprise-packages"
// For "path:chainguard-dev/mono/main/images/nginx" returns "chainguard-dev/mono"
// Returns empty string if the format is invalid.
func (e ExecutionContext) Repository() string {
	if e.ReconcilerKey == "" {
		return ""
	}

	// Split at colon to get the identifier part
	_, identifier, found := strings.Cut(e.ReconcilerKey, ":")
	if !found {
		return ""
	}

	// Find the second slash to extract "owner/repo"
	firstSlash := strings.IndexByte(identifier, '/')
	if firstSlash == -1 {
		return ""
	}

	secondSlash := strings.IndexByte(identifier[firstSlash+1:], '/')
	if secondSlash == -1 {
		return ""
	}

	return identifier[:firstSlash+1+secondSlash]
}

// EnrichAttributes adds execution context attributes to the provided base attributes.
// This is used to enrich metrics with reconciler context using only BOUNDED labels.
//
// Note: reconciler_key and commit_sha are NOT included in metrics to prevent unbounded
// cardinality (every PR and commit creates a new time series). These fields remain in
// the ExecutionContext for traces where cardinality is not a concern. Use trace exemplars
// to link from aggregated metrics to detailed per-PR traces.
func (e ExecutionContext) EnrichAttributes(baseAttrs []attribute.KeyValue) []attribute.KeyValue {
	// Pre-allocate for base + up to 3 additional attributes
	attrs := make([]attribute.KeyValue, len(baseAttrs), len(baseAttrs)+3)
	copy(attrs, baseAttrs)

	// Add reconciler type (bounded: "pr" or "path")
	if e.ReconcilerType != "" {
		attrs = append(attrs, attribute.String("reconciler_type", e.ReconcilerType))
	}

	// Extract and add repository from reconciler_key for aggregation
	// This is bounded: ~100-500 repositories vs unlimited PRs
	if repo := e.Repository(); repo != "" {
		attrs = append(attrs, attribute.String("repository", repo))
	}

	// Add turn number (bounded: typically 0-10 for multi-turn agents)
	attrs = append(attrs, attribute.Int("turn", e.TurnNumber))

	return attrs
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

// contextKey is used for storing execution context in context.Context
type contextKey string

const executionContextKey contextKey = "execution_context"

// WithExecutionContext adds execution context to the Go context
func WithExecutionContext(ctx context.Context, execCtx ExecutionContext) context.Context {
	return context.WithValue(ctx, executionContextKey, execCtx)
}

// GetExecutionContext retrieves execution context from the Go context
func GetExecutionContext(ctx context.Context) ExecutionContext {
	if val := ctx.Value(executionContextKey); val != nil {
		if execCtx, ok := val.(ExecutionContext); ok {
			return execCtx
		}
	}
	return ExecutionContext{}
}

// newTraceWithTracer creates a new trace with the given tracer and prompt
func newTraceWithTracer[T any](ctx context.Context, tracer Tracer[T], prompt string) *Trace[T] {
	// Extract execution context from Go context
	execCtx := GetExecutionContext(ctx)

	tr := otel.Tracer("chainguard.ai.agents.evals",
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
	tr := otel.Tracer("chainguard.ai.agents.evals",
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
	tr := otel.Tracer("chainguard.ai.agents.evals",
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
