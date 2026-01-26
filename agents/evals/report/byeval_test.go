/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report_test

import (
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/report"
)

func TestByEval(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Simulate some results following /{model}/{test case}/{eval} pattern
	// Model: claude, Test case: build-failure, Eval: no-errors
	evalObs1 := obs.Child("claude").Child("build-failure").Child("no-errors")
	evalObs1.Fail("compilation error")
	evalObs1.Increment()
	evalObs1.Increment()

	// Model: claude, Test case: test-failure, Eval: no-errors
	evalObs2 := obs.Child("claude").Child("test-failure").Child("no-errors")
	evalObs2.Increment()

	// Model: gemini, Test case: build-failure, Eval: no-errors
	evalObs3 := obs.Child("gemini").Child("build-failure").Child("no-errors")
	evalObs3.Fail("syntax error")
	evalObs3.Increment()

	// Model: claude, Test case: build-failure, Eval: has-reasoning
	evalObs4 := obs.Child("claude").Child("build-failure").Child("has-reasoning")
	evalObs4.Grade(0.9, "good reasoning")
	evalObs4.Increment()

	// Model: gemini, Test case: build-failure, Eval: has-reasoning
	evalObs5 := obs.Child("gemini").Child("build-failure").Child("has-reasoning")
	evalObs5.Grade(0.7, "okay reasoning")
	evalObs5.Increment()

	// Generate ByEval report
	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Check that we got some report content
	if reportStr == "" {
		t.Error("Expected non-empty report string")
	}

	// Verify report structure contains eval names in tree
	if !strings.Contains(reportStr, "no-errors") {
		t.Error("Expected report to contain 'no-errors' in tree")
	}
	if !strings.Contains(reportStr, "has-reasoning") {
		t.Error("Expected report to contain 'has-reasoning' in tree")
	}

	// Verify report contains model names in tree
	if !strings.Contains(reportStr, "claude") {
		t.Error("Expected report to contain 'claude' in tree")
	}
	if !strings.Contains(reportStr, "gemini") {
		t.Error("Expected report to contain 'gemini' in tree")
	}

	// Verify report contains test case names in tree
	if !strings.Contains(reportStr, "build-failure") {
		t.Error("Expected report to contain 'build-failure' in tree")
	}

	// Should detect failures since we have some failing evaluations
	if !hasFailure {
		t.Error("Expected hasFailure to be true with failing evaluations")
	}

	// Print report for debugging
	t.Logf("Generated report:\n%s", reportStr)
}

func TestByEvalNoFailures(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Simulate all passing results
	evalObs1 := obs.Child("claude").Child("test1").Child("eval1")
	evalObs1.Increment()

	evalObs2 := obs.Child("gemini").Child("test1").Child("eval1")
	evalObs2.Grade(0.95, "excellent")
	evalObs2.Increment()

	// Generate ByEval report with high threshold
	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Should not detect failures since all are passing
	if hasFailure {
		t.Error("Expected hasFailure to be false with all passing evaluations")
	}

	// Should still have eval name but no model/test case details (since they're all passing)
	if !strings.Contains(reportStr, "eval1") {
		t.Error("Expected report to contain 'eval1' in tree")
	}

	// Print report for debugging
	t.Logf("Generated report (no failures):\n%s", reportStr)
}

func TestByEvalInvalidPaths(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add some results that don't follow the /{model}/{test case}/{eval} pattern
	obs.Child("invalid").Increment()
	obs.Child("too").Child("many").Child("path").Child("components").Increment()

	// Generate ByEval report
	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Should be empty since no valid paths
	if reportStr != "" {
		t.Errorf("report: got = %s, wanted = empty", reportStr)
	}
	if hasFailure {
		t.Error("hasFailure: got = true, wanted = false with no valid paths")
	}
}

func TestByEvalEmptyObserver(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer with no data
	obs := evals.NewNamespacedObserver(factory)

	// Generate report
	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Should be empty
	if reportStr != "" {
		t.Errorf("report: got = %s, wanted = empty", reportStr)
	}
	if hasFailure {
		t.Error("hasFailure: got = true, wanted = false with empty observer")
	}
}

func TestByEvalZeroIterations(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Create child observers but don't increment them
	obs.Child("claude").Child("test1").Child("eval1").Fail("error")
	obs.Child("gemini").Child("test1").Child("eval1").Grade(0.5, "poor")

	// Generate report
	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Should be empty since no iterations were recorded
	if reportStr != "" {
		t.Errorf("report: got = %s, wanted = empty", reportStr)
	}
	if hasFailure {
		t.Error("hasFailure: got = true, wanted = false with zero iterations")
	}
}

func TestByEvalThresholdBehavior(t *testing.T) {
	tests := []struct {
		name      string
		threshold float64
		wantFail  bool
	}{{
		name:      "low threshold should pass",
		threshold: 0.5,
		wantFail:  false,
	}, {
		name:      "high threshold should fail",
		threshold: 0.9,
		wantFail:  true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a factory that creates test observers
			factory := func(name string) *evals.ResultCollector {
				return evals.NewResultCollector(&mockObserver{})
			}

			// Create root observer
			obs := evals.NewNamespacedObserver(factory)

			// Add results with 75% pass rate and 0.75 avg grade
			evalObs := obs.Child("claude").Child("test1").Child("eval1")
			evalObs.Increment()   // pass
			evalObs.Increment()   // pass
			evalObs.Increment()   // pass
			evalObs.Fail("error") // fail
			evalObs.Increment()
			evalObs.Grade(0.75, "good")

			_, hasFailure := report.ByEval(obs, tt.threshold)

			if hasFailure != tt.wantFail {
				t.Errorf("hasFailure: got = %v, wanted = %v", hasFailure, tt.wantFail)
			}
		})
	}
}

func TestByEvalGradeOnlyResults(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add only grade results (no pass/fail)
	evalObs := obs.Child("claude").Child("test1").Child("eval1")
	evalObs.Grade(0.6, "below threshold")
	evalObs.Increment()

	reportStr, hasFailure := report.ByEval(obs, 0.8)

	// Should detect failure due to low grade
	if !hasFailure {
		t.Error("Expected hasFailure to be true with grade below threshold")
	}

	// Should contain grade information in report
	if !strings.Contains(reportStr, "avg") {
		t.Error("Expected report to contain 'avg' for grade-only results")
	}

	// Should contain details about the low grade
	if !strings.Contains(reportStr, "below threshold") {
		t.Error("Expected report to contain grade reasoning")
	}
}

func TestByEvalSummaryTable(t *testing.T) {
	// Create a factory that creates test observers
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add fake results following /{model}/{test case}/{eval} pattern
	// This simulates the judge-reasoning and judge-suggestions evaluations

	// judge-reasoning results
	evalObs1 := obs.Child("gpt-4o").Child("clarity_confusing").Child("judge-reasoning")
	evalObs1.Grade(0.70, "Some confusion")
	evalObs1.Increment()

	evalObs2 := obs.Child("gpt-4o").Child("conciseness_poor").Child("judge-reasoning")
	evalObs2.Grade(0.25, "Too verbose")
	evalObs2.Increment()

	evalObs3 := obs.Child("gpt-4o").Child("factual_accuracy_incorrect").Child("judge-reasoning")
	evalObs3.Grade(1.00, "Perfect")
	evalObs3.Increment()

	evalObs4 := obs.Child("gpt-4o-mini").Child("clarity_confusing").Child("judge-reasoning")
	evalObs4.Grade(0.65, "Some confusion")
	evalObs4.Increment()

	evalObs5 := obs.Child("gpt-4o-mini").Child("conciseness_poor").Child("judge-reasoning")
	evalObs5.Grade(0.30, "Too verbose")
	evalObs5.Increment()

	evalObs6 := obs.Child("gpt-4o-mini").Child("factual_accuracy_incorrect").Child("judge-reasoning")
	evalObs6.Grade(0.90, "Good")
	evalObs6.Increment()

	// judge-suggestions results
	evalObs7 := obs.Child("gpt-4o").Child("clarity_confusing").Child("judge-suggestions")
	evalObs7.Grade(0.60, "Needs improvement")
	evalObs7.Increment()

	evalObs8 := obs.Child("gpt-4o").Child("conciseness_poor").Child("judge-suggestions")
	evalObs8.Grade(0.75, "Acceptable")
	evalObs8.Increment()

	evalObs9 := obs.Child("gpt-4o").Child("relevance_off_topic").Child("judge-suggestions")
	evalObs9.Grade(0.00, "Completely off")
	evalObs9.Increment()

	evalObs10 := obs.Child("gpt-4o-mini").Child("clarity_confusing").Child("judge-suggestions")
	evalObs10.Grade(0.55, "Needs improvement")
	evalObs10.Increment()

	evalObs11 := obs.Child("gpt-4o-mini").Child("conciseness_poor").Child("judge-suggestions")
	evalObs11.Grade(0.70, "Acceptable")
	evalObs11.Increment()

	evalObs12 := obs.Child("gpt-4o-mini").Child("relevance_off_topic").Child("judge-suggestions")
	evalObs12.Grade(0.00, "Completely off")
	evalObs12.Increment()

	// Generate ByEval report with summary table
	threshold := 0.8
	reportStr, hasFailure := report.ByEval(obs, threshold)

	// Should detect failures due to low grades
	if !hasFailure {
		t.Error("Expected hasFailure to be true with grades below threshold")
	}

	// Verify summary table is present
	if !strings.Contains(reportStr, "## Summary Table") {
		t.Error("Expected report to contain '## Summary Table' header")
	}

	// Verify table structure
	if !strings.Contains(reportStr, "| Evaluation Metric") {
		t.Error("Expected report to contain table header")
	}

	// Verify hierarchical structure with indentation
	if !strings.Contains(reportStr, "├─ clarity_confusing") {
		t.Error("Expected report to contain hierarchical indentation with ├─")
	}
	if !strings.Contains(reportStr, "└─") {
		t.Error("Expected report to contain hierarchical indentation with └─")
	}

	// Verify red cross marks for values below threshold
	if !strings.Contains(reportStr, "❌") {
		t.Error("Expected report to contain red cross marks for values below threshold")
	}

	// Verify both target evaluations are present
	if !strings.Contains(reportStr, "judge-reasoning") {
		t.Error("Expected report to contain judge-reasoning evaluation")
	}
	if !strings.Contains(reportStr, "judge-suggestions") {
		t.Error("Expected report to contain judge-suggestions evaluation")
	}

	// Verify model columns are present
	if !strings.Contains(reportStr, "gpt-4o") {
		t.Error("Expected report to contain gpt-4o model column")
	}
	if !strings.Contains(reportStr, "gpt-4o-mini") {
		t.Error("Expected report to contain gpt-4o-mini model column")
	}

	// Verify average column is present
	if !strings.Contains(reportStr, "Average") {
		t.Error("Expected report to contain Average column")
	}

	// Print report for debugging
	t.Logf("Generated report with summary table:\n%s", reportStr)
}
