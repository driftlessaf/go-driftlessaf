/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"testing"
)

func TestWithTracer(t *testing.T) {
	ctx := context.Background()
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}

	// Add tracer to context
	ctxWithTracer := WithTracer[string](ctx, tracer)

	// Retrieve tracer from context
	if retrieved := TracerFromContext[string](ctxWithTracer); retrieved != tracer {
		t.Errorf("retrieved tracer: got = %v, wanted = %v", retrieved, tracer)
	}

	// Test with context without tracer - should return default tracer
	if retrieved := TracerFromContext[string](ctx); retrieved == nil {
		t.Error("retrieved tracer from empty context: got = nil, wanted = default tracer")
	}
}

func TestStartTrace(t *testing.T) {
	ctx := context.Background()

	// Test without tracer in context - should still work with default tracer
	if trace := StartTrace[string](ctx, randomString()); trace == nil {
		t.Error("start trace without explicit tracer: got = nil, wanted = non-nil trace")
	}

	// Test with tracer in context
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}
	ctx = WithTracer[string](ctx, tracer)

	prompt := randomString()
	if trace := StartTrace[string](ctx, prompt); trace == nil {
		t.Fatal("start trace with tracer in context: got = nil, wanted = non-nil trace")
	} else if trace.InputPrompt != prompt {
		t.Errorf("trace prompt: got = %q, wanted = %q", trace.InputPrompt, prompt)
	}
}

func TestAutoRecordTrace(t *testing.T) {
	ctx := context.Background()
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}
	ctx = WithTracer[string](ctx, tracer)

	// Create and record a trace
	trace := StartTrace[string](ctx, randomString())
	if trace == nil {
		t.Fatal("start trace: got = nil, wanted = non-nil trace")
	}

	tc := trace.StartToolCall("tc1", randomString(), nil)
	tc.Complete(randomString(), nil)

	// Should not be recorded yet
	if len(traces) != 0 {
		t.Errorf("traces before completion: got = %d, wanted = 0", len(traces))
	}

	// Complete the trace - this should auto-record
	trace.Complete(randomString(), nil)

	// Check that trace was auto-recorded
	if len(traces) != 1 {
		t.Fatalf("traces after completion: got = %d, wanted = 1", len(traces))
	}

	if recorded := traces[0]; recorded != trace {
		t.Errorf("recorded trace: got = %v, wanted = %v", recorded, trace)
	}
}

func TestMultipleTracersWithDifferentTypes(t *testing.T) {
	ctx := context.Background()

	// Create tracers for different result types using the same generic type
	var stringTraces []*Trace[string]
	var intTraces []*Trace[int]

	stringTracer := &mockTracer[string]{traces: &stringTraces}
	intTracer := &mockTracer[int]{traces: &intTraces}

	// Add both tracers to the same context using different type parameters
	ctx = WithTracer[string](ctx, stringTracer)
	ctx = WithTracer[int](ctx, intTracer)

	// Verify we can retrieve each tracer independently
	retrievedStringTracer := TracerFromContext[string](ctx)
	retrievedIntTracer := TracerFromContext[int](ctx)

	if retrievedStringTracer != stringTracer {
		t.Errorf("retrieved string tracer: got = %v, wanted = %v", retrievedStringTracer, stringTracer)
	}

	if retrievedIntTracer != intTracer {
		t.Errorf("retrieved int tracer: got = %v, wanted = %v", retrievedIntTracer, intTracer)
	}

	// Create traces using each tracer
	stringTrace := StartTrace[string](ctx, randomString())
	intTrace := StartTrace[int](ctx, randomString())

	// Complete traces with appropriate types
	stringResult := randomString()
	stringTrace.Complete(stringResult, nil)
	intTrace.Complete(42, nil)

	// Verify traces were recorded by the correct tracers
	if len(stringTraces) != 1 {
		t.Fatalf("string traces count: got = %d, wanted = 1", len(stringTraces))
	}

	if len(intTraces) != 1 {
		t.Fatalf("int traces count: got = %d, wanted = 1", len(intTraces))
	}

	// Verify result types
	if stringTraces[0].Result != stringResult {
		t.Errorf("string trace result: got = %v, wanted = %q", stringTraces[0].Result, stringResult)
	}

	if intTraces[0].Result != 42 {
		t.Errorf("int trace result: got = %v, wanted = 42", intTraces[0].Result)
	}
}

// mockTracer is a generic test implementation of Tracer[T]
type mockTracer[T any] struct {
	traces *[]*Trace[T]
}

func (m *mockTracer[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return newTraceWithTracer[T](ctx, m, prompt)
}

func (m *mockTracer[T]) RecordTrace(trace *Trace[T]) {
	*m.traces = append(*m.traces, trace)
}
