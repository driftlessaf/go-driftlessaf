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

func ExampleLLMTurn_RecordReasoningTokens() {
	tracer := agenttrace.ByCode[string](func(trace *agenttrace.Trace[string]) {
		turn := trace.Turns[0]
		fmt.Printf("output=%d reasoning-subset=%d\n", turn.OutputTokens, turn.ReasoningTokens)
	})
	ctx := agenttrace.WithTracer[string](context.Background(), tracer)
	trace, done := agenttrace.StartTrace[string](ctx, "synthetic fixture")
	turn := trace.BeginTurn(0, "fixture-model", "fixture-provider")
	turn.RecordTokens(100, 30)
	turn.RecordReasoningTokens(10)
	turn.End()
	done("done", nil)
	// Output: output=30 reasoning-subset=10
}

func ExampleLLMTurn_RecordServingContext() {
	tracer := agenttrace.ByCode[string](func(trace *agenttrace.Trace[string]) {
		fmt.Println(trace.Turns[0].ServingLocation)
	})
	ctx := agenttrace.WithTracer[string](context.Background(), tracer)
	trace, done := agenttrace.StartTrace[string](ctx, "synthetic fixture")
	turn := trace.BeginTurn(0, "fixture-provider", "fixture-model")
	if err := turn.RecordServingContext(agenttrace.ServingContext{Location: "us-east-1"}); err != nil {
		panic(err)
	}
	turn.End()
	done("done", nil)
	// Output: us-east-1
}

func ExampleServingContext_Validate() {
	context := agenttrace.ServingContext{Location: "us-east-1"}
	fmt.Println(context.Validate())
	// Output: <nil>
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

// ExampleWithExecutionContext_partial demonstrates the merge-on-non-zero
// semantics: a deep call site that only knows about TurnNumber updates it
// without clobbering ReconcilerKey, ReconcilerType, or CommitSHA set by the
// enclosing reconciler.
func ExampleWithExecutionContext_partial() {
	ctx := agenttrace.WithExecutionContext(context.Background(), agenttrace.ExecutionContext{
		ReconcilerKey:  "pr:chainguard-dev/mono/40044",
		ReconcilerType: "pr",
		CommitSHA:      "abc123",
	})

	ctx = agenttrace.WithExecutionContext(ctx, agenttrace.ExecutionContext{TurnNumber: 3})

	ec := agenttrace.GetExecutionContext(ctx)
	fmt.Printf("key=%s sha=%s turn=%d\n", ec.ReconcilerKey, ec.CommitSHA, ec.TurnNumber)
	// Output: key=pr:chainguard-dev/mono/40044 sha=abc123 turn=3
}
