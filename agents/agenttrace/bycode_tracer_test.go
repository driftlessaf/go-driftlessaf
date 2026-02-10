/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestByCode(t *testing.T) {
	ctx := context.Background()
	var capturedTrace *Trace[string]

	// Create a callback that captures the trace
	callback := func(trace *Trace[string]) {
		capturedTrace = trace
	}

	// Create a tracer with the callback
	tracer := ByCode[string](callback)

	// Create and complete a trace
	prompt := randomString()
	trace := tracer.NewTrace(ctx, prompt)

	// Add a tool call
	toolName := randomString()
	tc := trace.StartToolCall("tc1", toolName, map[string]any{"key": "value"})
	toolResult := randomString()
	tc.Complete(toolResult, nil)

	// Complete the trace
	finalResult := randomString()
	trace.Complete(finalResult, nil)

	// Verify the callback was invoked
	if capturedTrace == nil {
		t.Fatal("callback invocation: got = nil, wanted = trace")
	}

	if capturedTrace != trace {
		t.Errorf("captured trace: got = %v, wanted = %v", capturedTrace, trace)
	}

	if capturedTrace.InputPrompt != prompt {
		t.Errorf("captured trace prompt: got = %q, wanted = %q", capturedTrace.InputPrompt, prompt)
	}

	if len(capturedTrace.ToolCalls) != 1 {
		t.Errorf("captured trace tool calls: got = %d, wanted = 1", len(capturedTrace.ToolCalls))
	}

	if capturedTrace.Result != finalResult {
		t.Errorf("captured trace result: got = %v, wanted = %q", capturedTrace.Result, finalResult)
	}
}

func TestByCodeWithNilCallback(t *testing.T) {
	ctx := context.Background()

	// Create a tracer with nil callback
	tracer := ByCode[string](nil)

	// Should not panic
	trace := tracer.NewTrace(ctx, randomString())

	// Complete should not panic even with nil callback
	result := randomString()
	trace.Complete(result, nil)
}

func TestByCodeWithMultipleCallbacks(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	capturedTraces := make([]*Trace[string], 3)
	callbackExecuted := make([]bool, 3)

	// Create multiple callbacks
	callback1 := func(trace *Trace[string]) {
		mu.Lock()
		capturedTraces[0] = trace
		callbackExecuted[0] = true
		mu.Unlock()
	}

	callback2 := func(trace *Trace[string]) {
		mu.Lock()
		capturedTraces[1] = trace
		callbackExecuted[1] = true
		mu.Unlock()
	}

	callback3 := func(trace *Trace[string]) {
		mu.Lock()
		capturedTraces[2] = trace
		callbackExecuted[2] = true
		mu.Unlock()
	}

	// Create a tracer with multiple callbacks
	tracer := ByCode[string](callback1, callback2, callback3)

	// Create and complete a trace
	prompt := randomString()
	trace := tracer.NewTrace(ctx, prompt)

	result := randomString()
	trace.Complete(result, nil)

	// Verify they all received the same trace
	for i, captured := range capturedTraces {
		if captured != trace {
			t.Errorf("Callback %d received different trace", i+1)
		}
	}

	// Verify all callbacks executed
	for i, executed := range callbackExecuted {
		if !executed {
			t.Errorf("Callback %d was not executed", i+1)
		}
	}
}

func TestByCodeWithNoCallbacks(t *testing.T) {
	ctx := context.Background()

	// Create a tracer with no callbacks
	tracer := ByCode[string]()

	// Should not panic
	trace := tracer.NewTrace(ctx, randomString())

	// Complete should not panic
	result := randomString()
	trace.Complete(result, nil)
}

func TestByCodeParallelExecution(t *testing.T) {
	ctx := context.Background()

	// Channel to signal when callbacks start
	started := make(chan int, 3)
	// Channel to control callback completion
	proceed := make(chan struct{})

	// Create callbacks that block until signaled
	callback1 := func(trace *Trace[string]) {
		started <- 1
		<-proceed
	}

	callback2 := func(trace *Trace[string]) {
		started <- 2
		<-proceed
	}

	callback3 := func(trace *Trace[string]) {
		started <- 3
		<-proceed
	}

	// Create a tracer with multiple callbacks
	tracer := ByCode[string](callback1, callback2, callback3)

	// Create and complete a trace
	prompt := randomString()
	trace := tracer.NewTrace(ctx, prompt)

	// Complete the trace in a goroutine so we can verify parallel execution
	done := make(chan struct{})
	go func() {
		result := randomString()
		trace.Complete(result, nil)
		close(done)
	}()

	// Verify all callbacks start executing (should happen quickly if parallel)
	timeout := time.After(100 * time.Millisecond)
	for range 3 {
		select {
		case <-started:
			// Good, a callback started
		case <-timeout:
			t.Fatal("Callbacks did not start in parallel")
		}
	}

	// Allow callbacks to complete
	close(proceed)

	// Wait for trace completion
	select {
	case <-done:
		// Good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Trace completion did not finish")
	}
}
