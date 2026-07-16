/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package evals_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/evals"
)

// mockObserver implements Observer for testing
type mockObserver struct {
	failures []string
	logs     []string
	count    int64
}

func (m *mockObserver) Fail(msg string) {
	m.failures = append(m.failures, msg)
}

func (m *mockObserver) Log(msg string) {
	m.logs = append(m.logs, msg)
}

func (m *mockObserver) Grade(score float64, reasoning string) {
	m.logs = append(m.logs, fmt.Sprintf("Grade: %.2f - %s", score, reasoning))
}

func (m *mockObserver) Increment() {
	m.count++
}

func (m *mockObserver) Total() int64 {
	return m.count
}

func TestExactToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		n           int
		toolCalls   int
		expectFail  bool
		failMessage string
	}{{
		name:       "exact match",
		n:          2,
		toolCalls:  2,
		expectFail: false,
	}, {
		name:        "too few",
		n:           3,
		toolCalls:   2,
		expectFail:  true,
		failMessage: "tool call count: got = 2, wanted = 3",
	}, {
		name:        "too many",
		n:           1,
		toolCalls:   2,
		expectFail:  true,
		failMessage: "tool call count: got = 2, wanted = 1",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			trace := &agenttrace.Trace[string]{
				ToolCalls: make([]*agenttrace.ToolCall[string], tt.toolCalls),
			}

			callback := evals.ExactToolCalls[string](tt.n)
			callback(obs, trace)

			if tt.expectFail {
				if len(obs.failures) == 0 {
					t.Errorf("failures: got = 0, wanted > 0")
				} else if obs.failures[0] != tt.failMessage {
					t.Errorf("failure message: got = %q, wanted = %q", obs.failures[0], tt.failMessage)
				}
			} else {
				if len(obs.failures) > 0 {
					t.Errorf("unexpected failure: %v", obs.failures)
				}
			}
		})
	}
}

func TestMinimumNToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		n           int
		toolCalls   int
		expectFail  bool
		failMessage string
	}{{
		name:       "exact match",
		n:          2,
		toolCalls:  2,
		expectFail: false,
	}, {
		name:       "more than minimum",
		n:          2,
		toolCalls:  3,
		expectFail: false,
	}, {
		name:        "less than minimum",
		n:           3,
		toolCalls:   2,
		expectFail:  true,
		failMessage: "tool call count: got = 2, wanted >= 3",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			trace := &agenttrace.Trace[string]{
				ToolCalls: make([]*agenttrace.ToolCall[string], tt.toolCalls),
			}

			callback := evals.MinimumNToolCalls[string](tt.n)
			callback(obs, trace)

			if tt.expectFail {
				if len(obs.failures) == 0 {
					t.Errorf("failures: got = 0, wanted > 0")
				} else if obs.failures[0] != tt.failMessage {
					t.Errorf("failure message: got = %q, wanted = %q", obs.failures[0], tt.failMessage)
				}
			} else {
				if len(obs.failures) > 0 {
					t.Errorf("unexpected failure: %v", obs.failures)
				}
			}
		})
	}
}

func TestOnlyToolCalls(t *testing.T) {
	obs := &mockObserver{}
	trace := &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs"},
			{Name: "analyze"},
			{Name: "summarize"},
		},
	}

	// Test with allowed tools
	callback := evals.OnlyToolCalls[string]("read_logs", "analyze", "summarize")
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with allowed tools: %v", obs.failures)
	}

	// Test with disallowed tool
	obs = &mockObserver{}
	callback = evals.OnlyToolCalls[string]("read_logs", "analyze")
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for disallowed tool")
	} else if !strings.Contains(obs.failures[0], "summarize") {
		t.Errorf("failure message: got = %q, wanted = contains 'summarize'", obs.failures[0])
	}
}

func TestOnlyToolCallsRejectsUnlistedToolAlongsideSubmit(t *testing.T) {
	// A genuinely unlisted tool fails, even when submit_result is allowed.
	obs := &mockObserver{}
	trace := &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "submit_result"},
			{Name: "rm_rf"},
		},
	}
	callback := evals.OnlyToolCalls[string]("read_logs", "submit_result")
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for disallowed tool rm_rf")
	} else if !strings.Contains(obs.failures[0], "rm_rf") {
		t.Errorf("failure message: got = %q, wanted = contains 'rm_rf'", obs.failures[0])
	}
}

func TestRequiredToolCalls(t *testing.T) {
	obs := &mockObserver{}
	trace := &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs"},
			{Name: "analyze"},
		},
	}

	// Test with all required tools present
	callback := evals.RequiredToolCalls[string]([]string{"read_logs", "analyze"})
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with all required tools: %v", obs.failures)
	}

	// Test with missing required tool
	obs = &mockObserver{}
	callback = evals.RequiredToolCalls[string]([]string{"read_logs", "analyze", "summarize"})
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for missing required tool")
	} else if !strings.Contains(obs.failures[0], "summarize") {
		t.Errorf("failure message: got = %q, wanted = contains 'summarize'", obs.failures[0])
	}
}

func TestNoErrors(t *testing.T) {
	// Test with no errors
	obs := &mockObserver{}
	trace := &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs"},
			{Name: "analyze"},
		},
	}

	callback := evals.NoErrors[string]()
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with no errors: %v", obs.failures)
	}

	// Test with trace error
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("trace failed"),
	}
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for trace error")
	}

	// Test with tool call error
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs", Error: errors.New("read failed")},
		},
	}
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for tool call error")
	}

	// Test with ignored tool call error
	obs = &mockObserver{}
	ignoreRead := func(err error) bool {
		return strings.Contains(err.Error(), "read failed")
	}
	callbackWithIgnore := evals.NoErrors[string](ignoreRead)
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with ignored error: %v", obs.failures)
	}

	// Test with ignored trace error
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("read failed"),
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with ignored trace error: %v", obs.failures)
	}

	// Test that non-matching errors still fail with ignore
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "write_file", Error: errors.New("write failed")},
		},
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for non-ignored error")
	}
}

func TestRangeToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		min         int
		max         int
		toolCalls   int
		expectFail  bool
		failMessage string
	}{{
		name:       "within range",
		min:        2,
		max:        4,
		toolCalls:  3,
		expectFail: false,
	}, {
		name:       "at minimum",
		min:        2,
		max:        4,
		toolCalls:  2,
		expectFail: false,
	}, {
		name:       "at maximum",
		min:        2,
		max:        4,
		toolCalls:  4,
		expectFail: false,
	}, {
		name:        "below minimum",
		min:         2,
		max:         4,
		toolCalls:   1,
		expectFail:  true,
		failMessage: "tool call count: got = 1, wanted = 2..4",
	}, {
		name:        "above maximum",
		min:         2,
		max:         4,
		toolCalls:   5,
		expectFail:  true,
		failMessage: "tool call count: got = 5, wanted = 2..4",
	}, {
		name:       "single value range",
		min:        3,
		max:        3,
		toolCalls:  3,
		expectFail: false,
	}, {
		name:        "single value range fail",
		min:         3,
		max:         3,
		toolCalls:   2,
		expectFail:  true,
		failMessage: "tool call count: got = 2, wanted = 3..3",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			trace := &agenttrace.Trace[string]{
				ToolCalls: make([]*agenttrace.ToolCall[string], tt.toolCalls),
			}

			callback := evals.RangeToolCalls[string](tt.min, tt.max)
			callback(obs, trace)

			if tt.expectFail {
				if len(obs.failures) == 0 {
					t.Errorf("failures: got = 0, wanted > 0")
				} else if obs.failures[0] != tt.failMessage {
					t.Errorf("failure message: got = %q, wanted = %q", obs.failures[0], tt.failMessage)
				}
			} else {
				if len(obs.failures) > 0 {
					t.Errorf("unexpected failure: %v", obs.failures)
				}
			}
		})
	}
}

func TestNoToolCalls(t *testing.T) {
	tests := []struct {
		name        string
		toolCalls   int
		expectFail  bool
		failMessage string
	}{{
		name:       "no tool calls",
		toolCalls:  0,
		expectFail: false,
	}, {
		name:        "one tool call",
		toolCalls:   1,
		expectFail:  true,
		failMessage: "tool call count: got = 1, wanted = 0",
	}, {
		name:        "multiple tool calls",
		toolCalls:   3,
		expectFail:  true,
		failMessage: "tool call count: got = 3, wanted = 0",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			trace := &agenttrace.Trace[string]{
				ToolCalls: make([]*agenttrace.ToolCall[string], tt.toolCalls),
			}

			callback := evals.NoToolCalls[string]()
			callback(obs, trace)

			if tt.expectFail {
				if len(obs.failures) == 0 {
					t.Errorf("failures: got = 0, wanted > 0")
				} else if obs.failures[0] != tt.failMessage {
					t.Errorf("failure message: got = %q, wanted = %q", obs.failures[0], tt.failMessage)
				}
			} else {
				if len(obs.failures) > 0 {
					t.Errorf("unexpected failure: %v", obs.failures)
				}
			}
		})
	}
}
