/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"chainguard.dev/driftlessaf/agents/evals"
)

// Evals returns a map of evaluation name to evaluation function for the specified judge mode.
// These evaluations focus on judgment quality, reasoning accuracy, and appropriate scoring for the given mode.
func Evals(mode JudgmentMode) map[string]evals.ObservableTraceCallback[*Judgement] {
	return map[string]evals.ObservableTraceCallback[*Judgement]{
		// Global evaluations for all models
		"no-errors":     evals.NoErrors[*Judgement](),
		"valid-score":   ValidScore(mode),
		"check-mode":    CheckMode(mode),
		"has-reasoning": HasReasoning(),
		"no-tool-calls": evals.NoToolCalls[*Judgement](),
	}
}
