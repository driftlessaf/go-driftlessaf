/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTraceConcurrentToolCalls(t *testing.T) {
	tracer := ByCode[string]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	// Number of concurrent tool calls
	numCalls := 100
	var wg sync.WaitGroup
	wg.Add(numCalls)

	// Start tool calls concurrently
	for idx := range numCalls {
		go func(idx int) {
			defer wg.Done()

			// Create tool call ID
			id := fmt.Sprintf("tc-%d", idx)
			name := fmt.Sprintf("tool-%d", idx)

			// Randomly choose between StartToolCall and BadToolCall
			if idx%2 == 0 {
				tc := trace.StartToolCall(id, name, map[string]any{"index": idx})
				// Simulate some work
				time.Sleep(time.Microsecond)
				tc.Complete(map[string]any{"result": idx}, nil)
			} else {
				trace.BadToolCall(id, name, map[string]any{"index": idx}, fmt.Errorf("bad call %d", idx))
			}
		}(idx)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify all tool calls were recorded
	if len(trace.ToolCalls) != numCalls {
		t.Errorf("tool calls count: got = %d, wanted = %d", len(trace.ToolCalls), numCalls)
	}

	// Verify each tool call is present and valid
	seen := make(map[string]struct{}, numCalls)
	for _, tc := range trace.ToolCalls {
		if _, exists := seen[tc.ID]; exists {
			t.Errorf("duplicate tool call ID: got = %s (already seen), wanted = unique", tc.ID)
		}
		seen[tc.ID] = struct{}{}

		// Verify tool call has proper times set
		if tc.StartTime.IsZero() || tc.EndTime.IsZero() {
			t.Errorf("Tool call %s has invalid times", tc.ID)
		}
	}
}

func TestTraceConcurrentComplete(t *testing.T) {
	// Number of concurrent traces
	numTraces := 50
	recordedTraces := make([]*Trace[string], 0, numTraces)
	var mu sync.Mutex

	// Create a tracer that records traces
	tracer := ByCode[string](func(trace *Trace[string]) {
		mu.Lock()
		recordedTraces = append(recordedTraces, trace)
		mu.Unlock()
	})
	var wg sync.WaitGroup
	wg.Add(numTraces)

	// Create and complete traces concurrently
	for idx := range numTraces {
		go func(idx int) {
			defer wg.Done()

			ctx := context.Background()
			trace := tracer.NewTrace(ctx, fmt.Sprintf("trace-%d", idx))

			// Add some tool calls
			for j := range 5 {
				tc := trace.StartToolCall(
					fmt.Sprintf("tc-%d-%d", idx, j),
					fmt.Sprintf("tool-%d", j),
					nil,
				)
				tc.Complete(fmt.Sprintf("result-%d-%d", idx, j), nil)
			}

			// Complete the trace
			trace.Complete(fmt.Sprintf("final-%d", idx), nil)
		}(idx)
	}

	// Wait for all goroutines
	wg.Wait()

	// Verify all traces were recorded
	if len(recordedTraces) != numTraces {
		t.Errorf("recorded traces count: got = %d, wanted = %d", len(recordedTraces), numTraces)
	}

	// Verify each trace has the expected tool calls
	for _, trace := range recordedTraces {
		if len(trace.ToolCalls) != 5 {
			t.Errorf("trace %s tool calls: got = %d, wanted = 5", trace.ID, len(trace.ToolCalls))
		}
	}
}

func TestToolCallConcurrentAccess(t *testing.T) {
	tracer := ByCode[string]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	// Create a tool call
	tc := trace.StartToolCall("tc1", randomString(), nil)
	result := randomString()

	var wg sync.WaitGroup

	// Reader goroutines
	for range 10 {
		wg.Go(func() {
			for range 100 {
				_ = tc.Duration()
				time.Sleep(time.Microsecond)
			}
		})
	}

	// Writer goroutine
	wg.Go(func() {
		time.Sleep(5 * time.Millisecond)
		tc.Complete(result, nil)
	})

	// Wait for all goroutines
	wg.Wait()

	// Verify the tool call was completed properly
	if tc.Result != result {
		t.Errorf("tool call result: got = %v, wanted = %q", tc.Result, result)
	}
	if tc.EndTime.IsZero() {
		t.Error("tool call end time: got = zero time, wanted = set time")
	}
}

func TestTraceDurationConcurrentAccess(t *testing.T) {
	tracer := ByCode[string]() // No callbacks
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())
	result := randomString()

	var wg sync.WaitGroup

	// Reader goroutines
	for range 10 {
		wg.Go(func() {
			for range 100 {
				_ = trace.Duration()
				time.Sleep(time.Microsecond)
			}
		})
	}

	// Writer goroutine
	wg.Go(func() {
		time.Sleep(5 * time.Millisecond)
		trace.Complete(result, nil)
	})

	// Wait for all goroutines
	wg.Wait()

	// Verify the trace was completed properly
	if trace.Result != result {
		t.Errorf("trace result: got = %v, wanted = %q", trace.Result, result)
	}
	if trace.EndTime.IsZero() {
		t.Error("trace end time: got = zero time, wanted = set time")
	}
}
