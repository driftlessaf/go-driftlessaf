/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/judge"
)

func TestGoldenEvals(t *testing.T) {
	evals := judge.Evals(judge.GoldenMode)

	// Verify we have the expected evaluations
	expectedEvals := []string{
		"no-errors",
		"valid-score",
		"check-mode",
		"has-reasoning",
		"no-tool-calls",
	}

	if len(evals) != len(expectedEvals) {
		t.Errorf("eval count: got = %d, wanted = %d", len(evals), len(expectedEvals))
	}

	for _, expected := range expectedEvals {
		if _, exists := evals[expected]; !exists {
			t.Errorf("missing expected eval: %s", expected)
		}
	}
}

func TestBenchmarkEvals(t *testing.T) {
	evals := judge.Evals(judge.BenchmarkMode)

	// Verify we have the expected evaluations
	expectedEvals := []string{
		"no-errors",
		"valid-score",
		"check-mode",
		"has-reasoning",
		"no-tool-calls",
	}

	if len(evals) != len(expectedEvals) {
		t.Errorf("eval count: got = %d, wanted = %d", len(evals), len(expectedEvals))
	}

	for _, expected := range expectedEvals {
		if _, exists := evals[expected]; !exists {
			t.Errorf("missing expected eval: %s", expected)
		}
	}
}
