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

// mockObserver implements evals.Observer for testing
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
	// Mock implementation does nothing with grades
}

func (m *mockObserver) Increment() {
	m.count++
}

func (m *mockObserver) Total() int64 {
	return m.count
}

func TestSimpleReportBasic(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add some basic test data
	child1 := obs.Child("test1")
	child1.Fail("error 1")
	child1.Fail("error 2")
	child1.Increment()
	child1.Increment()
	child1.Increment()

	child2 := obs.Child("test2")
	child2.Increment()
	child2.Increment()

	// Generate report with 0.8 threshold
	reportStr, hasFailure := report.Simple(obs, 0.8)

	// Debug: print the actual report
	t.Logf("Generated report:\n%s", reportStr)

	// Should have failure since test1 has 1/3 pass rate (33%) < 80%
	if !hasFailure {
		t.Error("hasFailure: got = false, wanted = true")
	}

	// Check report contains expected tree structure
	if !strings.Contains(reportStr, "test1") {
		t.Error("report should contain test1 in tree")
	}
	if !strings.Contains(reportStr, "test2") {
		t.Error("report should contain test2 in tree")
	}
	if !strings.Contains(reportStr, "❌") {
		t.Error("report should contain failure indicator for test1")
	}
	if !strings.Contains(reportStr, "33.3%") {
		t.Error("report should contain test1 pass rate")
	}
	if !strings.Contains(reportStr, "100.0%") {
		t.Error("report should contain test2 pass rate")
	}
	if !strings.Contains(reportStr, "error 1") {
		t.Error("report should contain failure messages")
	}
	if !strings.Contains(reportStr, "[FAIL]") {
		t.Error("report should contain FAIL indicators")
	}
}

func TestSimpleReportWithGrades(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add test with grades
	child := obs.Child("graded-test")
	child.Grade(0.9, "excellent work")
	child.Grade(0.7, "good effort")
	child.Grade(0.5, "needs improvement")
	child.Increment()
	child.Increment()
	child.Increment()

	// Generate report with 0.8 threshold
	reportStr, hasFailure := report.Simple(obs, 0.8)

	// Should have failure since average grade (0.7) < 0.8 and one grade (0.5) < 0.8
	if !hasFailure {
		t.Error("hasFailure: got = false, wanted = true")
	}

	// Check report contains grade information
	if !strings.Contains(reportStr, "0.70 avg") {
		t.Error("report should contain average grade")
	}
	if !strings.Contains(reportStr, "0.50") {
		t.Error("report should contain below-threshold grade score")
	}
	if !strings.Contains(reportStr, "needs improvement") {
		t.Error("report should contain grade reasoning")
	}
}

func TestSimpleReportWithBothPassRateAndGrades(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add test with both failures and grades
	child := obs.Child("mixed-test")
	child.Fail("some error")
	child.Grade(0.85, "good work")
	child.Grade(0.95, "excellent")
	child.Increment()
	child.Increment()
	child.Increment()

	// Generate report with 0.8 threshold
	reportStr, hasFailure := report.Simple(obs, 0.8)

	// Should have failure since pass rate (2/3 = 66.7%) < 80%
	if !hasFailure {
		t.Error("hasFailure: got = false, wanted = true")
	}

	// Check report shows both metrics
	if !strings.Contains(reportStr, "66.7% pass") {
		t.Error("report should contain pass rate")
	}
	if !strings.Contains(reportStr, "0.90 avg") {
		t.Error("report should contain average grade")
	}
}

func TestSimpleReportNoFailures(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add successful test
	child := obs.Child("success-test")
	child.Grade(0.9, "great work")
	child.Increment()
	child.Increment()

	// Generate report with 0.8 threshold
	reportStr, hasFailure := report.Simple(obs, 0.8)

	// Should not have failure
	if hasFailure {
		t.Error("hasFailure: got = true, wanted = false")
	}

	// Check report format - when there are no failures, only grade is shown
	if !strings.Contains(reportStr, "1 result") {
		t.Error("report should show grade count when no failures")
	}
	if !strings.Contains(reportStr, "0.90 avg") {
		t.Error("report should show average grade")
	}
}

func TestSimpleReportEmptyNamespace(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer with no data
	obs := evals.NewNamespacedObserver(factory)

	// Add empty child
	obs.Child("empty-test")

	// Generate report
	reportStr, hasFailure := report.Simple(obs, 0.8)

	// Should not have failure for empty namespaces
	if hasFailure {
		t.Error("hasFailure: got = true, wanted = false for empty namespaces")
	}

	// Empty namespaces with no iterations should be skipped
	if strings.Contains(reportStr, "empty-test") {
		t.Error("report should not contain empty namespace with no iterations")
	}
}

func TestSimpleReportNestedNamespaces(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Create nested structure
	level1 := obs.Child("level1")
	level2 := level1.Child("level2")
	level3 := level2.Child("level3")

	level3.Increment()

	// Generate report
	reportStr, _ := report.Simple(obs, 0.8)

	// Check tree structure is correct
	if !strings.Contains(reportStr, "level1") {
		t.Error("report should contain level1 in tree")
	}
	if !strings.Contains(reportStr, "level2") {
		t.Error("report should contain level2 in tree")
	}
	if !strings.Contains(reportStr, "level3") {
		t.Error("report should contain level3 in tree")
	}
}

func TestSimpleReportSingularPluralGrades(t *testing.T) {
	// Create a factory that creates result collectors
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Test with single grade
	single := obs.Child("single-grade")
	single.Grade(0.9, "good")
	single.Increment()

	// Test with multiple grades
	multiple := obs.Child("multiple-grades")
	multiple.Grade(0.8, "okay")
	multiple.Grade(0.9, "good")
	multiple.Increment()
	multiple.Increment()

	// Generate report
	reportStr, _ := report.Simple(obs, 0.8)

	// Check singular/plural forms
	if !strings.Contains(reportStr, "1 result") {
		t.Error("report should use singular 'result' for single grade")
	}
	if !strings.Contains(reportStr, "2 results") {
		t.Error("report should use plural 'results' for multiple grades")
	}
}

func TestSimpleReportMatchesGeneratorSignature(t *testing.T) {
	// Verify Simple function matches Generator type signature
	var generator report.Generator = report.Simple

	// Create minimal test
	factory := func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(&mockObserver{})
	}
	obs := evals.NewNamespacedObserver(factory)

	// Should be callable as Generator
	_, _ = generator(obs, 0.8)
}
