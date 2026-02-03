/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// randomString generates a random string for testing
func randomString() string {
	return fmt.Sprintf("test-%d", rand.Int63())
}

func TestNewTrace(t *testing.T) {
	prompt := randomString()
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, prompt)

	if trace == nil {
		t.Fatal("got = nil, wanted = non-nil trace")
	}

	if trace.InputPrompt != prompt {
		t.Errorf("prompt: got = %q, wanted = %q", trace.InputPrompt, prompt)
	}

	if trace.ID == "" {
		t.Error("trace ID: got = empty string, wanted = non-empty")
	}

	if trace.StartTime.IsZero() {
		t.Error("start time: got = zero time, wanted = set time")
	}

	if len(trace.ToolCalls) != 0 {
		t.Errorf("tool calls length: got = %d, wanted = 0", len(trace.ToolCalls))
	}

	if trace.Metadata == nil {
		t.Error("metadata: got = nil, wanted = initialized map")
	}
}

func TestTraceStartToolCall(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	params := map[string]any{
		"param1": "value1",
		"param2": 42,
	}
	result := map[string]any{
		"status": "success",
	}

	// Start a tool call
	toolName := randomString()
	tc := trace.StartToolCall("tc1", toolName, params)
	if tc == nil {
		t.Fatal("StartToolCall should return a non-nil *ToolCall")
	}

	if tc.Name != toolName {
		t.Errorf("tool name: got = %q, wanted = %q", tc.Name, toolName)
	}

	// Tool call should not be added to trace yet
	if len(trace.ToolCalls) != 0 {
		t.Errorf("tool calls length: got = %d, wanted = 0 (before completion)", len(trace.ToolCalls))
	}

	// Complete the tool call
	tc.Complete(result, nil)

	// Now it should be added
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after completion: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	if recordedTC := trace.ToolCalls[0]; recordedTC.Name != toolName {
		t.Errorf("recorded tool name: got = %q, wanted = %q", recordedTC.Name, toolName)
	} else if recordedTC.Error != nil {
		t.Errorf("recorded tool error: got = %v, wanted = nil", recordedTC.Error)
	}

	// Test tool call with error
	err := errors.New("test error")
	tc2 := trace.StartToolCall("tc2", "error-tool", nil)
	tc2.Complete(nil, err)

	if len(trace.ToolCalls) != 2 {
		t.Fatalf("tool calls length after second completion: got = %d, wanted = 2", len(trace.ToolCalls))
	}

	if recordedTC2 := trace.ToolCalls[1]; !errors.Is(recordedTC2.Error, err) {
		t.Errorf("second tool call error: got = %v, wanted = %v", recordedTC2.Error, err)
	}
}

func TestToolCallDuration(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	tc := trace.StartToolCall("tc1", randomString(), nil)

	// Test duration before completion
	time.Sleep(10 * time.Millisecond)
	duration1 := tc.Duration()
	if duration1 == 0 {
		t.Error("incomplete tool call duration: got = 0, wanted = non-zero")
	}

	// Complete the tool call
	result := randomString()
	tc.Complete(result, nil)
	duration2 := tc.Duration()
	if duration2 == 0 {
		t.Error("completed tool call duration: got = 0, wanted = non-zero")
	}

	// Duration should be consistent after completion
	time.Sleep(10 * time.Millisecond)
	duration3 := tc.Duration()
	if duration2 != duration3 {
		t.Errorf("duration consistency: got = %v, wanted = %v", duration3, duration2)
	}
}

func TestTraceComplete(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	// Sleep briefly to ensure EndTime is different from StartTime
	time.Sleep(10 * time.Millisecond)

	result := randomString()
	trace.Complete(result, nil)

	if trace.Result != result {
		t.Errorf("trace result: got = %v, wanted = %v", trace.Result, result)
	}

	if trace.Error != nil {
		t.Errorf("trace error: got = %v, wanted = nil", trace.Error)
	}

	if trace.EndTime.IsZero() {
		t.Error("end time: got = zero time, wanted = set time")
	}

	if trace.Duration() == 0 {
		t.Error("trace duration: got = 0, wanted = non-zero")
	}
}

func TestTraceCompleteWithError(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	err := errors.New("test error")
	trace.Complete("", err)

	if !errors.Is(trace.Error, err) {
		t.Errorf("trace error: got = %v, wanted = %v", trace.Error, err)
	}

	if !trace.EndTime.IsZero() && trace.EndTime.Before(trace.StartTime) {
		t.Errorf("end time order: got = %v before start time %v, wanted = after", trace.EndTime, trace.StartTime)
	}
}

func TestTraceDuration(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	// Test duration before completion
	time.Sleep(10 * time.Millisecond)
	duration1 := trace.Duration()
	if duration1 == 0 {
		t.Error("incomplete trace duration: got = 0, wanted = non-zero")
	}

	// Test duration after completion
	trace.Complete(randomString(), nil)
	duration2 := trace.Duration()
	if duration2 == 0 {
		t.Error("completed trace duration: got = 0, wanted = non-zero")
	}

	// Duration should be consistent after completion
	time.Sleep(10 * time.Millisecond)
	duration3 := trace.Duration()
	if duration2 != duration3 {
		t.Errorf("duration consistency: got = %v, wanted = %v", duration3, duration2)
	}
}

func TestGenerateTraceID(t *testing.T) {
	// Test that trace IDs are unique
	ids := make(map[string]struct{}, 100)
	for range 100 {
		id := generateTraceID()
		if id == "" {
			t.Error("generated ID: got = empty string, wanted = non-empty")
		}
		if _, exists := ids[id]; exists {
			t.Errorf("duplicate ID: got = %s (already seen), wanted = unique", id)
		}
		ids[id] = struct{}{}
	}
}

func TestBadToolCall(t *testing.T) {
	tracer := &mockTracer[string]{traces: &[]*Trace[string]{}}
	ctx := context.Background()
	trace := tracer.NewTrace(ctx, randomString())

	// Test BadToolCall
	err := errors.New("invalid parameters")
	trace.BadToolCall("bad-tc-1", "bad-tool", map[string]any{
		"invalid": "params",
	}, err)

	// Check that the bad tool call was added
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("tool calls length after BadToolCall: got = %d, wanted = 1", len(trace.ToolCalls))
	}

	badTC := trace.ToolCalls[0]
	if badTC.ID != "bad-tc-1" {
		t.Errorf("bad tool call ID: got = %q, wanted = %q", badTC.ID, "bad-tc-1")
	}
	if badTC.Name != "bad-tool" {
		t.Errorf("bad tool call name: got = %q, wanted = %q", badTC.Name, "bad-tool")
	}
	if !errors.Is(badTC.Error, err) {
		t.Errorf("bad tool call error: got = %v, wanted = %v", badTC.Error, err)
	}
	if badTC.Result != nil {
		t.Errorf("bad tool call result: got = %v, wanted = nil", badTC.Result)
	}
	if badTC.StartTime.IsZero() || badTC.EndTime.IsZero() {
		t.Errorf("bad tool call times: start = %v, end = %v, wanted = both non-zero", badTC.StartTime, badTC.EndTime)
	}
	// The times should be very close (within a millisecond)
	duration := badTC.EndTime.Sub(badTC.StartTime)
	if duration > time.Millisecond {
		t.Errorf("bad tool call duration: got = %v, wanted = < 1ms", duration)
	}
}

func TestTraceStringEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		setupFn  func() *Trace[string]
		contains []string // strings that should be in the output
		excludes []string // strings that should NOT be in the output
	}{{
		name: "empty trace",
		setupFn: func() *Trace[string] {
			return (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "")
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"\"",
			"No tool calls",
			"Result: ",
		},
	}, {
		name: "trace with very long prompt",
		setupFn: func() *Trace[string] {
			longPrompt := strings.Repeat("a", 1000)
			return (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), longPrompt)
		},
		contains: []string{
			"=== Trace",
			strings.Repeat("a", 1000), // Should include full prompt
			"No tool calls",
		},
	}, {
		name: "trace with very long result",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "test")
			longResult := strings.Repeat("b", 1000)
			trace.Complete(longResult, nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"test\"",
			"No tool calls",
			"Result: " + strings.Repeat("b", 497) + "...", // Should be truncated at 500 chars
		},
		excludes: []string{
			strings.Repeat("b", 1000), // Full result should not appear
		},
	}, {
		name: "trace with metadata",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "test with metadata")
			trace.Metadata["custom_key"] = "custom_value"
			trace.Metadata["number"] = 42
			trace.Complete("done", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"test with metadata\"",
			"Metadata:",
			"custom_key: custom_value",
			"number: 42",
		},
	}, {
		name: "trace with tool calls having long results",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "test")
			tc := trace.StartToolCall("tc1", "test-tool", map[string]any{
				"param1": "value1",
				"param2": 123,
			})
			longResult := strings.Repeat("c", 300)
			tc.Complete(longResult, nil)
			trace.Complete("final", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (1):",
			"[1] test-tool (ID: tc1)",
			"param1: value1",
			"param2: 123",
			"Result: " + strings.Repeat("c", 197) + "...", // Tool call result truncated at 200 chars
		},
		excludes: []string{
			strings.Repeat("c", 300), // Full tool call result should not appear
		},
	}, {
		name: "trace with failed tool calls",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "test")
			tc := trace.StartToolCall("tc1", "failing-tool", nil)
			tc.Complete(nil, errors.New("tool failed"))
			trace.Complete("partial success", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (1):",
			"[1] failing-tool (ID: tc1)",
			"Error: tool failed",
			"Result: partial success",
		},
	}, {
		name: "trace with mixed tool call states",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "mixed test")

			// Successful tool call
			tc1 := trace.StartToolCall("tc1", "success-tool", map[string]any{
				"key": "value",
			})
			tc1.Complete("success result", nil)

			// Failed tool call
			tc2 := trace.StartToolCall("tc2", "fail-tool", nil)
			tc2.Complete(nil, errors.New("failed"))

			// Tool call with nil result (no error)
			tc3 := trace.StartToolCall("tc3", "nil-tool", nil)
			tc3.Complete(nil, nil)

			trace.Complete("mixed results", nil)
			return trace
		},
		contains: []string{
			"=== Trace",
			"Tool Calls (3):",
			"[1] success-tool (ID: tc1)",
			"key: value",
			"Result: success result",
			"[2] fail-tool (ID: tc2)",
			"Error: failed",
			"[3] nil-tool (ID: tc3)",
			"Result: mixed results",
		},
	}, {
		name: "trace that never completes",
		setupFn: func() *Trace[string] {
			trace := (&mockTracer[string]{traces: &[]*Trace[string]{}}).NewTrace(context.Background(), "incomplete")
			tc := trace.StartToolCall("tc1", "incomplete-tool", nil)
			// Don't complete the tool call or trace
			_ = tc
			return trace
		},
		contains: []string{
			"=== Trace",
			"Prompt: \"incomplete\"",
			"No tool calls",
			"Result: ",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := tt.setupFn()
			output := trace.String()

			// Check required strings are present
			for _, required := range tt.contains {
				if !strings.Contains(output, required) {
					t.Errorf("Expected output to contain %q, but it didn't.\nFull output:\n%s", required, output)
				}
			}

			// Check excluded strings are not present
			for _, excluded := range tt.excludes {
				if strings.Contains(output, excluded) {
					t.Errorf("Expected output to NOT contain %q, but it did.\nFull output:\n%s", excluded, output)
				}
			}

			// Basic structure checks
			if !strings.Contains(output, "=== Trace") {
				t.Error("Output should contain trace header")
			}
			if !strings.Contains(output, "Duration:") {
				t.Error("Output should contain duration information")
			}
		})
	}
}
