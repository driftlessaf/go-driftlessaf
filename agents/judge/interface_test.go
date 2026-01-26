/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/judge"
)

func TestJudgementString(t *testing.T) {
	tests := []struct {
		name     string
		judgment *judge.Judgement
		expected string
	}{{
		name: "basic judgment with score and reasoning",
		judgment: &judge.Judgement{
			Score:     0.85,
			Reasoning: "Good response with minor issues",
		},
		expected: "Grade: 0.85 - Good response with minor issues",
	}, {
		name: "perfect score no reasoning",
		judgment: &judge.Judgement{
			Score: 1.0,
		},
		expected: "Grade: 1.00",
	}, {
		name: "score with suggestions",
		judgment: &judge.Judgement{
			Score: 0.75,
			Suggestions: []string{
				"Add more detail",
				"Be more specific",
			},
		},
		expected: "Grade: 0.75\n  Suggestion: Add more detail\n  Suggestion: Be more specific",
	}, {
		name: "complete judgment with score, reasoning, and suggestions",
		judgment: &judge.Judgement{
			Score:     0.60,
			Reasoning: "Response lacks detail and clarity",
			Suggestions: []string{
				"Include specific examples",
				"Clarify the main points",
				"Improve sentence structure",
			},
		},
		expected: "Grade: 0.60 - Response lacks detail and clarity\n  Suggestion: Include specific examples\n  Suggestion: Clarify the main points\n  Suggestion: Improve sentence structure",
	}, {
		name: "zero score with full details",
		judgment: &judge.Judgement{
			Score:     0.0,
			Reasoning: "Completely incorrect response",
			Suggestions: []string{
				"Review the source material",
			},
		},
		expected: "Grade: 0.00 - Completely incorrect response\n  Suggestion: Review the source material",
	}, {
		name: "empty judgment",
		judgment: &judge.Judgement{
			Score: 0.5,
		},
		expected: "Grade: 0.50",
	}, {
		name: "empty reasoning with suggestions",
		judgment: &judge.Judgement{
			Score: 0.8,
			Suggestions: []string{
				"Minor improvement needed",
			},
		},
		expected: "Grade: 0.80\n  Suggestion: Minor improvement needed",
	}, {
		name: "empty suggestions with reasoning",
		judgment: &judge.Judgement{
			Score:       0.9,
			Reasoning:   "Nearly perfect response",
			Suggestions: []string{},
		},
		expected: "Grade: 0.90 - Nearly perfect response",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.judgment.String()
			if result != tt.expected {
				t.Errorf("String() result: got = %q, wanted = %q", result, tt.expected)
			}
		})
	}
}
