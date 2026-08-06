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
	failures     []string
	logs         []string
	grades       []float64
	gradeReasons []string
	count        int64
}

func (m *mockObserver) Fail(msg string) {
	m.failures = append(m.failures, msg)
}

func (m *mockObserver) Log(msg string) {
	m.logs = append(m.logs, msg)
}

func (m *mockObserver) Grade(score float64, reasoning string) {
	m.grades = append(m.grades, score)
	m.gradeReasons = append(m.gradeReasons, reasoning)
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
	// A clean run neither fails nor grades.
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
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade with no errors: %v", obs.gradeReasons)
	}

	// A trace-level error fails: the run died before reaching a result.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("trace failed"),
	}
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for trace error")
	}

	// A tool-call error on a completed run is a recovered transient: no
	// failure, but a Grade carrying the error in its reasoning.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs", Error: errors.New("read failed")},
			{Name: "read_logs"},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for recovered tool call error: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}
	if obs.grades[0] != 1.0 {
		t.Errorf("grade score: got = %v, wanted = 1.0", obs.grades[0])
	}
	if !strings.Contains(obs.gradeReasons[0], "read_logs: read failed") {
		t.Errorf("grade reasoning: got = %q, wanted = contains 'read_logs: read failed'", obs.gradeReasons[0])
	}

	// An ignored tool-call error is skipped entirely: no failure, no grade.
	obs = &mockObserver{}
	ignoreRead := func(err error) bool {
		return strings.Contains(err.Error(), "read failed")
	}
	callbackWithIgnore := evals.NoErrors[string](ignoreRead)
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with ignored error: %v", obs.failures)
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade with ignored error: %v", obs.gradeReasons)
	}

	// An ignore function filters the Grade reasoning, not just the failure:
	// on a trace mixing an ignored error with a reported one, the reasoning
	// names only the reported one and the count excludes the ignored call.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs", Error: errors.New("read failed")},
			{Name: "write_file", Error: errors.New("permission denied")},
		},
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with one ignored error: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}
	reason := obs.gradeReasons[0]
	if !strings.Contains(reason, "recovered from 1 tool-call error(s)") {
		t.Errorf("grade reasoning: got = %q, wanted = contains 'recovered from 1 tool-call error(s)'", reason)
	}
	if !strings.Contains(reason, "write_file: permission denied") {
		t.Errorf("grade reasoning: got = %q, wanted = contains 'write_file: permission denied'", reason)
	}
	if strings.Contains(reason, "read failed") {
		t.Errorf("grade reasoning: got = %q, wanted = 'read failed' suppressed by the ignore func", reason)
	}

	// An ignored trace error does not fail.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("read failed"),
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure with ignored trace error: %v", obs.failures)
	}

	// A non-ignored trace error still fails with an ignore func present.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("write failed"),
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for non-ignored trace error")
	}

	// The Grade reasoning caps the listed errors and reports the total. One
	// clean call keeps this on the recovered path rather than the
	// no-successful-call failure below.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "edit_file", Error: errors.New("miss 1")},
			{Name: "edit_file", Error: errors.New("miss 2")},
			{Name: "edit_file", Error: errors.New("miss 3")},
			{Name: "edit_file", Error: errors.New("miss 4")},
			{Name: "edit_file", Error: errors.New("miss 5")},
			{Name: "edit_file"},
		},
	}
	callback(obs, trace)
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}
	reason = obs.gradeReasons[0]
	if !strings.Contains(reason, "recovered from 5 tool-call error(s)") {
		t.Errorf("grade reasoning: got = %q, wanted = contains 'recovered from 5 tool-call error(s)'", reason)
	}
	if !strings.Contains(reason, "and 2 more") {
		t.Errorf("grade reasoning: got = %q, wanted = contains 'and 2 more'", reason)
	}
	if strings.Contains(reason, "miss 4") {
		t.Errorf("grade reasoning: got = %q, wanted = 'miss 4' elided by the cap", reason)
	}

	// A recoverable rejection (a handler declined the call but returned a
	// corrective hint the model acted on, e.g. a resubmitted terminal submit)
	// is a designed correction, not tool misuse. It must not fail an
	// otherwise-clean trace and must not appear in the Grade reasoning, even
	// with no ignore functions configured. This is the case that failed the
	// skillup-skillfixer eval gate on 2026-07-20: a stringified submit
	// payload rejected with "parameter error", then resubmitted cleanly.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "submit_result", Error: errors.New("parameter error"), Recoverable: true},
			{Name: "submit_result"},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for recovered rejection: %v", obs.failures)
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade for recovered rejection: %v", obs.gradeReasons)
	}

	// A no-tool agent records the same two calls and nothing else.
	// cve-advisor and conformance run on toolcall.EmptyTools, so excluding
	// the terminal submit leaves succeeded at zero for every run they make.
	// Such a run must clear the gate on having no reportable error, never on
	// a successful call it cannot make.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "submit_result", Error: errors.New("parameter error"), Recoverable: true},
			{Name: "submit_result", Terminal: true},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for a no-tool agent's recovered submit: %v", obs.failures)
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade for a no-tool agent's recovered submit: %v", obs.gradeReasons)
	}

	// An UNRECOVERED rejection still fails: a run whose model never lands an
	// accepted submission cannot complete cleanly. It exhausts the turn
	// budget, which surfaces as a trace-level error the recoverable mark
	// does not shield.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Error: errors.New("agent exceeded maximum conversation turns (200)"),
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "submit_result", Error: errors.New("parameter error"), Recoverable: true},
		},
	}
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for unrecovered rejection (trace error)")
	}

	// An unmarked tool-call error is reported, not failed. A recovered blip
	// the producer did NOT mark recoverable (e.g. an edit_file old-string
	// miss) is still tool-misuse signal, so the Grade reasoning names it,
	// but it no longer gates the run.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "edit_file", Error: errors.New("old string appears 0 times in the file")},
			{Name: "submit_result"},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for unmarked tool call error: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}
	if want := "edit_file: old string appears 0 times in the file"; !strings.Contains(obs.gradeReasons[0], want) {
		t.Errorf("grade reasoning: got = %q, wanted = contains %q", obs.gradeReasons[0], want)
	}

	// An unknown tool name is the one tool-call error class that stays
	// terminal: the model named a tool the executor has no handler for, which
	// is a protocol violation no retry can make legal. Matched through the
	// sentinel, so the wrapped tool name in the message carries no weight.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_the_logs", Error: fmt.Errorf("%w: %q", agenttrace.ErrUnknownTool, "read_the_logs")},
			{Name: "submit_result"},
		},
	}
	callback(obs, trace)
	if len(obs.failures) == 0 {
		t.Errorf("expected failure for unknown tool call")
	}

	// The failure is the sentinel's doing, not the message's: the same
	// reasoning text on an ordinary error is reported, not failed.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_the_logs", Error: errors.New(`unknown tool: "read_the_logs"`)},
			{Name: "submit_result"},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for unwrapped unknown-tool text: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}

	// An ignore function still suppresses an unknown-tool error. A consumer
	// whose agent legitimately expects one owns that decision.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_the_logs", Error: fmt.Errorf("%w: %q", agenttrace.ErrUnknownTool, "read_the_logs")},
		},
	}
	evals.NoErrors[string](func(err error) bool {
		return errors.Is(err, agenttrace.ErrUnknownTool)
	})(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for ignored unknown tool call: %v", obs.failures)
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade for ignored unknown tool call: %v", obs.gradeReasons)
	}

	// Every work call errored and the run still committed a result: the
	// executors steer a stuck model to submit a degraded result, so the clean
	// completion proves nothing. The terminal submit is not a success, so
	// this fails rather than grades.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs", Error: errors.New("read failed")},
			{Name: "edit_file", Error: errors.New("old string appears 0 times in the file")},
			{Name: "submit_result", Terminal: true},
		},
	}
	callback(obs, trace)
	if len(obs.failures) != 1 {
		t.Fatalf("failure count: got = %d, wanted = 1", len(obs.failures))
	}
	if want := "no tool call succeeded; 2 tool-call error(s)"; !strings.Contains(obs.failures[0], want) {
		t.Errorf("failure message: got = %q, wanted = contains %q", obs.failures[0], want)
	}
	if !strings.Contains(obs.failures[0], "read_logs: read failed") {
		t.Errorf("failure message: got = %q, wanted = contains 'read_logs: read failed'", obs.failures[0])
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade with no successful tool call: %v", obs.gradeReasons)
	}

	// One work call errored and another succeeded: the agent worked past the
	// error, so the recovered path still grades. The terminal submit alongside
	// it changes nothing.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "edit_file", Error: errors.New("old string appears 0 times in the file")},
			{Name: "edit_file"},
			{Name: "submit_result", Terminal: true},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for recovered tool call error: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}
	if obs.grades[0] != 1.0 {
		t.Errorf("grade score: got = %v, wanted = 1.0", obs.grades[0])
	}

	// A call whose error an ignore function suppresses counts as a success:
	// the ignore func declares that answer usable, so the reported error
	// alongside it grades rather than fails.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "read_logs", Error: errors.New("read failed")},
			{Name: "write_file", Error: errors.New("permission denied")},
			{Name: "submit_result", Terminal: true},
		},
	}
	callbackWithIgnore(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure when an ignored error is the only success: %v", obs.failures)
	}
	if len(obs.grades) != 1 {
		t.Fatalf("grade count: got = %d, wanted = 1", len(obs.grades))
	}

	// A recoverable rejection is not a success: the handler declined the
	// call. A run whose only other call errored has worked past nothing, so
	// it fails even though the rejection itself goes unreported.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "edit_file", Error: errors.New("old string appears 0 times in the file")},
			{Name: "submit_result", Error: errors.New("parameter error"), Recoverable: true},
			{Name: "submit_result", Terminal: true},
		},
	}
	callback(obs, trace)
	if len(obs.failures) != 1 {
		t.Fatalf("failure count: got = %d, wanted = 1", len(obs.failures))
	}
	if want := "no tool call succeeded; 1 tool-call error(s)"; !strings.Contains(obs.failures[0], want) {
		t.Errorf("failure message: got = %q, wanted = contains %q", obs.failures[0], want)
	}

	// A suspended trace (halted mid-run to await a human answer) is a
	// non-error terminal state: NoErrors neither fails nor grades it, even
	// when an error surfaced through the tool-call channel (the suspension
	// sentinel is error-shaped). The same unmarked tool-call error on a
	// non-suspended trace does produce a Grade (see the read_logs case
	// above), so the absent grade here proves the Suspended short-circuit
	// does the suppressing, not the absence of errors.
	obs = &mockObserver{}
	trace = &agenttrace.Trace[string]{
		Suspended:        true,
		SuspensionReason: "awaiting answer",
		ToolCalls: []*agenttrace.ToolCall[string]{
			{Name: "ask_a_friend", Error: errors.New("suspended: awaiting answer")},
		},
	}
	callback(obs, trace)
	if len(obs.failures) > 0 {
		t.Errorf("unexpected failure for suspended trace: %v", obs.failures)
	}
	if len(obs.grades) > 0 {
		t.Errorf("unexpected grade for suspended trace: %v", obs.gradeReasons)
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
