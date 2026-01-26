/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/judge"
)

// Mock observer is defined in testhelpers_test.go

func TestValidScore(t *testing.T) {
	tests := []struct {
		name          string
		trace         *evals.Trace[*judge.Judgement]
		expectFailure bool
		expectedMsg   string
	}{{
		name: "valid score in range",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.5},
		},
		expectFailure: false,
	}, {
		name: "valid score at minimum boundary",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.0},
		},
		expectFailure: false,
	}, {
		name: "valid score at maximum boundary",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 1.0},
		},
		expectFailure: false,
	}, {
		name: "nil result",
		trace: &evals.Trace[*judge.Judgement]{
			Result: nil,
		},
		expectFailure: true,
		expectedMsg:   "result is nil",
	}, {
		name: "score below range",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: -0.1},
		},
		expectFailure: true,
		expectedMsg:   "score -0.10 is out of range [0, 1] for golden mode",
	}, {
		name: "score above range",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 1.5},
		},
		expectFailure: true,
		expectedMsg:   "score 1.50 is out of range [0, 1] for golden mode",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			eval := judge.ValidScore(judge.GoldenMode)
			eval(obs, tt.trace)

			failures := obs.getFailures()
			if tt.expectFailure {
				if len(failures) == 0 {
					t.Error("failure count: got = 0, wanted = 1")
				} else if failures[0] != tt.expectedMsg {
					t.Errorf("failure message: got = %s, wanted = %s", failures[0], tt.expectedMsg)
				}
			} else {
				if len(failures) > 0 {
					t.Errorf("failure count: got = %d, wanted = 0", len(failures))
				}
			}
		})
	}
}

func TestMinimumScore(t *testing.T) {
	tests := []struct {
		name          string
		threshold     float64
		trace         *evals.Trace[*judge.Judgement]
		expectFailure bool
		expectedMsg   string
	}{{
		name:      "score meets threshold exactly",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.8},
		},
		expectFailure: false,
	}, {
		name:      "score exceeds threshold",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.9},
		},
		expectFailure: false,
	}, {
		name:      "nil result",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: nil,
		},
		expectFailure: true,
		expectedMsg:   "judgment result is nil",
	}, {
		name:      "score below threshold",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.7},
		},
		expectFailure: true, // Expect grade < 1.0 for out-of-range score
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			eval := judge.ScoreRange(tt.threshold, 1.0)
			eval(obs, tt.trace)

			failures := obs.getFailures()
			grades := obs.getGrades()

			// Check for nil result failures first
			if tt.trace.Result == nil {
				if len(failures) == 0 {
					t.Error("failure count: got = 0, wanted = 1")
				} else if failures[0] != tt.expectedMsg {
					t.Errorf("failure message: got = %s, wanted = %s", failures[0], tt.expectedMsg)
				}
				return
			}

			// For non-nil results, should never fail but always grade
			if len(failures) > 0 {
				t.Errorf("unexpected failures: got = %v, wanted = []", failures)
			}

			if len(grades) != 1 {
				t.Errorf("grade count: got = %d, wanted = 1", len(grades))
				return
			}

			grade := grades[0].score

			// Verify grade is within bounds [0, 1]
			if grade < 0.0 || grade > 1.0 {
				t.Errorf("grade out of bounds: got = %f, wanted = [0.0, 1.0]", grade)
			}

			// New semantics: expectFailure means "expect grade < 1.0"
			if tt.expectFailure {
				if grade >= 1.0 {
					t.Errorf("expected grade < 1.0 for out-of-range score: got = %f", grade)
				}
			} else {
				if grade != 1.0 {
					t.Errorf("expected grade = 1.0 for in-range score: got = %f", grade)
				}
			}
		})
	}
}

func TestMaximumScore(t *testing.T) {
	tests := []struct {
		name          string
		threshold     float64
		trace         *evals.Trace[*judge.Judgement]
		expectFailure bool
		expectedMsg   string
	}{{
		name:      "score meets threshold exactly",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.8},
		},
		expectFailure: false,
	}, {
		name:      "score below threshold",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.7},
		},
		expectFailure: false,
	}, {
		name:      "nil result",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: nil,
		},
		expectFailure: true,
		expectedMsg:   "judgment result is nil",
	}, {
		name:      "score above threshold",
		threshold: 0.8,
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Score: 0.9},
		},
		expectFailure: true, // Expect grade < 1.0 for out-of-range score
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			eval := judge.ScoreRange(0.0, tt.threshold)
			eval(obs, tt.trace)

			failures := obs.getFailures()
			grades := obs.getGrades()

			// Check for nil result failures first
			if tt.trace.Result == nil {
				if len(failures) == 0 {
					t.Error("failure count: got = 0, wanted = 1")
				} else if failures[0] != tt.expectedMsg {
					t.Errorf("failure message: got = %s, wanted = %s", failures[0], tt.expectedMsg)
				}
				return
			}

			// For non-nil results, should never fail but always grade
			if len(failures) > 0 {
				t.Errorf("unexpected failures: got = %v, wanted = []", failures)
			}

			if len(grades) != 1 {
				t.Errorf("grade count: got = %d, wanted = 1", len(grades))
				return
			}

			grade := grades[0].score

			// Verify grade is within bounds [0, 1]
			if grade < 0.0 || grade > 1.0 {
				t.Errorf("grade out of bounds: got = %f, wanted = [0.0, 1.0]", grade)
			}

			// New semantics: expectFailure means "expect grade < 1.0"
			if tt.expectFailure {
				if grade >= 1.0 {
					t.Errorf("expected grade < 1.0 for out-of-range score: got = %f", grade)
				}
			} else {
				if grade != 1.0 {
					t.Errorf("expected grade = 1.0 for in-range score: got = %f", grade)
				}
			}
		})
	}
}

func TestHasReasoning(t *testing.T) {
	tests := []struct {
		name          string
		trace         *evals.Trace[*judge.Judgement]
		expectFailure bool
		expectedMsg   string
	}{{
		name: "has reasoning",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Reasoning: "This is the reasoning"},
		},
		expectFailure: false,
	}, {
		name: "nil result",
		trace: &evals.Trace[*judge.Judgement]{
			Result: nil,
		},
		expectFailure: true,
		expectedMsg:   "result is nil",
	}, {
		name: "empty reasoning",
		trace: &evals.Trace[*judge.Judgement]{
			Result: &judge.Judgement{Reasoning: ""},
		},
		expectFailure: true,
		expectedMsg:   "judgment has no reasoning",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			eval := judge.HasReasoning()
			eval(obs, tt.trace)

			failures := obs.getFailures()
			if tt.expectFailure {
				if len(failures) == 0 {
					t.Error("failure count: got = 0, wanted = 1")
				} else if failures[0] != tt.expectedMsg {
					t.Errorf("failure message: got = %s, wanted = %s", failures[0], tt.expectedMsg)
				}
			} else {
				if len(failures) > 0 {
					t.Errorf("failure count: got = %d, wanted = 0", len(failures))
				}
			}
		})
	}
}

func TestNoToolCalls(t *testing.T) {
	tests := []struct {
		name          string
		trace         *evals.Trace[*judge.Judgement]
		expectFailure bool
		expectedMsg   string
	}{{
		name: "no tool calls",
		trace: &evals.Trace[*judge.Judgement]{
			ToolCalls: nil,
		},
		expectFailure: false,
	}, {
		name: "empty tool calls slice",
		trace: &evals.Trace[*judge.Judgement]{
			ToolCalls: []*evals.ToolCall[*judge.Judgement]{},
		},
		expectFailure: false,
	}, {
		name: "one tool call",
		trace: &evals.Trace[*judge.Judgement]{
			ToolCalls: []*evals.ToolCall[*judge.Judgement]{
				{ID: "tool_call_1", Name: "test_tool"},
			},
		},
		expectFailure: true,
		expectedMsg:   "tool call count: got = 1, wanted = 0",
	}, {
		name: "multiple tool calls",
		trace: &evals.Trace[*judge.Judgement]{
			ToolCalls: []*evals.ToolCall[*judge.Judgement]{
				{ID: "tool_call_1", Name: "test_tool_1"},
				{ID: "tool_call_2", Name: "test_tool_2"},
				{ID: "tool_call_3", Name: "test_tool_3"},
			},
		},
		expectFailure: true,
		expectedMsg:   "tool call count: got = 3, wanted = 0",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &mockObserver{}
			eval := evals.NoToolCalls[*judge.Judgement]()
			eval(obs, tt.trace)

			failures := obs.getFailures()
			if tt.expectFailure {
				if len(failures) == 0 {
					t.Error("failure count: got = 0, wanted = 1")
				} else if failures[0] != tt.expectedMsg {
					t.Errorf("failure message: got = %s, wanted = %s", failures[0], tt.expectedMsg)
				}
			} else {
				if len(failures) > 0 {
					t.Errorf("failure count: got = %d, wanted = 0", len(failures))
				}
			}
		})
	}
}
