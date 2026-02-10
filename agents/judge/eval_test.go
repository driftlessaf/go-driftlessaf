/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/judge"
)

// Mock types are defined in testhelpers_test.go

func TestNewGoldenEval(t *testing.T) {
	// Create mock judge
	mockJudgment := &judge.Judgement{
		Score:     0.85,
		Reasoning: "Good match with minor differences",
		Suggestions: []string{
			"Consider being more specific",
			"Add more detail",
		},
	}
	judgeImpl := &mockJudge{judgment: mockJudgment}

	// Create the eval callback
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "correctness", "Expected answer")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "What is 2+2?",
		Result: &judge.Judgement{
			Score:     0.9,
			Reasoning: "The answer is correct",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// Check logs
	logs := obs.getLogs()
	if len(logs) == 0 {
		t.Error("log count: got = 0, wanted = > 0")
	}

	// Verify key log messages
	expectedLogs := []string{
		"Grade: 0.85",
		"Good match with minor differences",
		"Suggestion: Consider being more specific",
		"Suggestion: Add more detail",
	}

	for _, expected := range expectedLogs {
		found := false
		for _, log := range logs {
			if strings.Contains(log, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("log content: got = %v, wanted = containing %q", logs, expected)
		}
	}
}

func TestNewEvalWithError(t *testing.T) {
	// Create mock judge that returns error
	judgeImpl := &mockJudge{err: errors.New("API error")}

	// Create the eval callback
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "accuracy", "Expected answer")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result: &judge.Judgement{
			Score:     0.5,
			Reasoning: "Test reasoning",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// Should fail with the error
	logs := obs.getLogs()
	found := false
	for _, log := range logs {
		if strings.Contains(log, "Judge failed") && strings.Contains(log, "API error") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("failure log: got = %v, wanted = containing 'Judge failed' and 'API error'", logs)
	}
}

func TestNewEvalWithNilResult(t *testing.T) {
	judgeImpl := &mockJudge{}
	evalCallback := judge.NewGoldenEval[*judge.Judgement](judgeImpl, "completeness", "Expected")

	// Create trace with nil result
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result:      nil,
	}

	obs := &mockObserver{}
	evalCallback(obs, trace)

	// Should fail on nil result extraction
	logs := obs.getLogs()
	found := false
	for _, log := range logs {
		if strings.Contains(log, "Failed to extract response") && strings.Contains(log, "trace has no result") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extraction failure log: got = %v, wanted = containing 'Failed to extract response' and 'trace has no result'", logs)
	}
}

func TestNewStandaloneEval(t *testing.T) {
	// Create mock judge
	mockJudgment := &judge.Judgement{
		Score:     0.8,
		Reasoning: "Response is clear and well-structured",
		Suggestions: []string{
			"Add more specific examples",
		},
	}
	judgeImpl := &mockJudge{judgment: mockJudgment}

	// Create the eval callback
	evalCallback := judge.NewStandaloneEval[*judge.Judgement](judgeImpl, "clarity - response should be easy to understand")

	// Create a test trace
	trace := &agenttrace.Trace[*judge.Judgement]{
		InputPrompt: "Test prompt",
		Result: &judge.Judgement{
			Score:     0.7,
			Reasoning: "Test response for evaluation",
		},
	}

	// Create mock observer
	obs := &mockObserver{}

	// Run the eval
	evalCallback(obs, trace)

	// Check logs
	logs := obs.getLogs()
	if len(logs) == 0 {
		t.Error("log count: got = 0, wanted = > 0")
	}

	// Verify key log messages
	expectedLogs := []string{
		"Grade: 0.80",
		"Response is clear and well-structured",
		"Suggestion: Add more specific examples",
	}

	for _, expected := range expectedLogs {
		found := false
		for _, log := range logs {
			if strings.Contains(log, expected) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("log content: got = %v, wanted = containing %q", logs, expected)
		}
	}
}
