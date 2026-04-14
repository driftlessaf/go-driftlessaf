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
	ctx := t.Context()
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
	ctx := t.Context()

	// Test without tracer in context - should still work with default tracer
	if trace, _ := StartTrace[string](ctx, randomString()); trace == nil {
		t.Error("start trace without explicit tracer: got = nil, wanted = non-nil trace")
	}

	// Test with tracer in context
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}
	ctx = WithTracer[string](ctx, tracer)

	prompt := randomString()
	if trace, _ := StartTrace[string](ctx, prompt); trace == nil {
		t.Fatal("start trace with tracer in context: got = nil, wanted = non-nil trace")
	} else if trace.InputPrompt != prompt {
		t.Errorf("trace prompt: got = %q, wanted = %q", trace.InputPrompt, prompt)
	}
}

func TestAutoRecordTrace(t *testing.T) {
	ctx := t.Context()
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}
	ctx = WithTracer[string](ctx, tracer)

	// StartTrace returns the trace and a done callback that records it
	trace, done := StartTrace[string](ctx, randomString())
	if trace == nil {
		t.Fatal("start trace: got = nil, wanted = non-nil trace")
	}

	tc := trace.StartToolCall("tc1", randomString(), nil)
	tc.Complete(randomString(), nil)

	// Should not be recorded yet
	if len(traces) != 0 {
		t.Errorf("traces before done: got = %d, wanted = 0", len(traces))
	}

	// Calling done completes the trace and records it
	result := randomString()
	done(result, nil)

	// Check that trace was recorded
	if len(traces) != 1 {
		t.Fatalf("traces after done: got = %d, wanted = 1", len(traces))
	}

	if recorded := traces[0]; recorded != trace {
		t.Errorf("recorded trace: got = %v, wanted = %v", recorded, trace)
	}

	if trace.Result != result {
		t.Errorf("trace result: got = %q, wanted = %q", trace.Result, result)
	}
}

// TestCompleteDoesNotRecord verifies that calling complete alone fills in trace
// fields but does NOT trigger recording. Recording is the executor's job via
// the done callback from StartTrace.
func TestCompleteDoesNotRecord(t *testing.T) {
	ctx := t.Context()
	var traces []*Trace[string]
	tracer := &mockTracer[string]{traces: &traces}
	ctx = WithTracer[string](ctx, tracer)

	trace, _ := StartTrace[string](ctx, randomString())

	tc := trace.StartToolCall("tc1", randomString(), nil)
	tc.Complete(randomString(), nil)

	// complete fills in result but should not record
	trace.complete(randomString(), nil)

	if len(traces) != 0 {
		t.Errorf("traces after complete (not done): got = %d, wanted = 0", len(traces))
	}
}

func TestMultipleTracersWithDifferentTypes(t *testing.T) {
	ctx := t.Context()

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
	_, stringDone := StartTrace[string](ctx, randomString())
	_, intDone := StartTrace[int](ctx, randomString())

	// Complete traces with appropriate types via done callbacks
	stringResult := randomString()
	stringDone(stringResult, nil)
	intDone(42, nil)

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

// TestDecoratorRecordTraceComposition verifies that a decorator wrapping
// RecordTrace is called when a trace completes. This is the core contract
// for decorator composition: the outermost tracer (set on context by
// middleware) must be the one that receives the RecordTrace call, not
// the innermost leaf tracer that created the trace.
func TestDecoratorRecordTraceComposition(t *testing.T) {
	ctx := t.Context()

	var innerCount, decoratorCount int
	inner := &mockTracer[string]{traces: new([]*Trace[string])}
	inner.onRecord = func() { innerCount++ }

	decorator := &countingDecorator[string]{
		inner:    inner,
		onRecord: func() { decoratorCount++ },
	}

	// Middleware sets the outermost decorator on context
	ctx = WithTracer[string](ctx, decorator)

	_, done := StartTrace[string](ctx, "test prompt")
	done("done", nil)

	if decoratorCount != 1 {
		t.Errorf("decorator RecordTrace calls: got %d, want 1", decoratorCount)
	}
	if innerCount != 1 {
		t.Errorf("inner RecordTrace calls: got %d, want 1", innerCount)
	}
}

// countingDecorator delegates NewTrace to an inner tracer and wraps
// RecordTrace with a callback. This is the minimal decorator pattern
// that breaks without the context-based tracer lookup fix.
type countingDecorator[T any] struct {
	inner    Tracer[T]
	onRecord func()
}

func (d *countingDecorator[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return d.inner.NewTrace(ctx, prompt)
}

func (d *countingDecorator[T]) RecordTrace(trace *Trace[T]) {
	d.onRecord()
	d.inner.RecordTrace(trace)
}

// mockTracer is a generic test implementation of Tracer[T]
type mockTracer[T any] struct {
	traces   *[]*Trace[T]
	onRecord func()
}

func (m *mockTracer[T]) NewTrace(ctx context.Context, prompt string) *Trace[T] {
	return newTrace[T](ctx, prompt)
}

func (m *mockTracer[T]) RecordTrace(trace *Trace[T]) {
	if m.onRecord != nil {
		m.onRecord()
	}
	*m.traces = append(*m.traces, trace)
}
