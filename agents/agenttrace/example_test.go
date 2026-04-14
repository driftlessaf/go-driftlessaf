/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace_test

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
)

// ExampleStartTrace demonstrates creating and completing a trace.
func ExampleStartTrace() {
	ctx := context.Background()

	tracer := agenttrace.ByCode[string](func(trace *agenttrace.Trace[string]) {
		fmt.Printf("Trace completed: %s\n", trace.Result)
	})
	ctx = agenttrace.WithTracer[string](ctx, tracer)

	_, done := agenttrace.StartTrace[string](ctx, "Analyze the report")
	done("analysis done", nil)
	// Output: Trace completed: analysis done
}

// ExampleWithExecutionContext demonstrates attaching execution context to a
// context for trace enrichment.
func ExampleWithExecutionContext() {
	ctx := context.Background()
	ctx = agenttrace.WithExecutionContext(ctx, agenttrace.ExecutionContext{
		ReconcilerKey:  "pr:chainguard-dev/enterprise-packages/42",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
		TurnNumber:     1,
	})

	ec := agenttrace.GetExecutionContext(ctx)
	fmt.Printf("key=%s turn=%d\n", ec.ReconcilerKey, ec.TurnNumber)
	// Output: key=pr:chainguard-dev/enterprise-packages/42 turn=1
}
