/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package agenttrace provides tracing infrastructure for AI agent interactions.

# Overview

This package contains the foundational types for tracking agent executions:

  - ExecutionContext: Reconciler-level metadata (PR, path, commit, turn number) for trace enrichment
  - Trace[T]: Complete agent interaction from prompt to result
  - ToolCall[T]: Individual tool invocation within a trace
  - Tracer[T]: Interface for creating and managing traces

# Separation of Concerns

The agenttrace package provides low-level tracing primitives. Higher-level evaluation
helpers (NoErrors, ExactToolCalls, etc.), observers, and metrics reporters are in the
evals package which builds on top of this package.

# Usage

Set execution context for trace enrichment:

	ctx = agenttrace.WithExecutionContext(ctx, agenttrace.ExecutionContext{
		ReconcilerKey:  "pr:chainguard-dev/enterprise-packages/41025",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
		TurnNumber:     1,
	})

Create and use traces:

	tracer := agenttrace.ByCode[string](func(trace *agenttrace.Trace[string]) {
		log.Printf("Trace completed: %s", trace.ID)
	})
	ctx = agenttrace.WithTracer[string](ctx, tracer)

	trace := agenttrace.StartTrace[string](ctx, "Analyze the security report")
	toolCall := trace.StartToolCall("tc1", "file-reader", map[string]any{
		"path": "/var/logs/security.log",
	})
	toolCall.Complete("File content here", nil)
	trace.Complete("Analysis complete", nil)
*/
package agenttrace
