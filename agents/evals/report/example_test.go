/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report_test

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/evals/report"
)

// exampleObserver implements evals.Observer for examples
type exampleObserver struct {
	name  string
	count int64
}

func (e *exampleObserver) Fail(msg string) {
	// Silent for clean example output
}

func (e *exampleObserver) Log(msg string) {
	// Silent for clean example output
}

func (e *exampleObserver) Grade(score float64, reasoning string) {
	// Silent for clean example output
}

func (e *exampleObserver) Increment() {
	e.count++
}

func (e *exampleObserver) Total() int64 {
	return e.count
}

// ExampleSimple demonstrates basic usage of the Simple report generator.
func ExampleSimple() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add some evaluation data
	testObs := obs.Child("security-tests")
	testObs.Fail("Buffer overflow detected")
	testObs.Grade(0.7, "Some security issues found")
	testObs.Increment()
	testObs.Increment()

	// Generate simple report with 80% threshold
	reportStr, hasFailures := report.Simple(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: true
	// Report:
	// security-tests [❌ 50.0% pass, 0.70 avg] (1/2)
	// ├ 1 [FAIL] Buffer overflow detected
	// └ 2 [0.70] Some security issues found
}

// ExampleSimple_successfulEvaluation demonstrates a successful evaluation with no failures.
func ExampleSimple_successfulEvaluation() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add successful evaluation data
	testObs := obs.Child("quality-check")
	testObs.Grade(0.95, "Excellent code quality")
	testObs.Grade(0.88, "Good test coverage")
	testObs.Increment()
	testObs.Increment()

	// Generate report with 80% threshold
	reportStr, hasFailures := report.Simple(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: false
	// Report:
	// quality-check [0.92 avg] (2 results)
}

// ExampleSimple_nestedNamespaces demonstrates hierarchical namespace reporting.
func ExampleSimple_nestedNamespaces() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Create nested evaluation structure
	frontend := obs.Child("frontend")
	frontendSecurity := frontend.Child("security")
	frontendSecurity.Grade(0.85, "Good security practices")
	frontendSecurity.Increment()

	backend := obs.Child("backend")
	backendSecurity := backend.Child("security")
	backendSecurity.Fail("SQL injection vulnerability")
	backendSecurity.Increment()

	// Generate report
	reportStr, _ := report.Simple(obs, 0.8)

	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Report:
	// backend
	// └ security [❌ 0.0%] (0/1)
	//   └ 1 [FAIL] SQL injection vulnerability
	// frontend
	// └ security [0.85 avg] (1 result)
}

// ExampleGenerator demonstrates using the Generator function type.
func ExampleGenerator() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer with test data
	obs := evals.NewNamespacedObserver(factory)
	testObs := obs.Child("api-tests")
	testObs.Grade(0.9, "All endpoints working")
	testObs.Increment()

	// Use the Generator function type
	var generator report.Generator = report.Simple

	// Generate report using the function type
	reportStr, hasFailures := generator(obs, 0.8)

	fmt.Printf("Using Generator function type\n")
	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report contains: %t\n", len(reportStr) > 0)

	// Output:
	// Using Generator function type
	// Has failures: false
	// Report contains: true
}

// ExampleSimple_multipleMetrics demonstrates both pass rates and grades in the same report.
func ExampleSimple_multipleMetrics() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add test with both failures and grades
	testObs := obs.Child("integration-tests")
	testObs.Fail("Timeout in payment service")
	testObs.Grade(0.85, "Most functionality works")
	testObs.Grade(0.92, "Good error handling")
	testObs.Increment()
	testObs.Increment()
	testObs.Increment()

	// Generate report
	reportStr, hasFailures := report.Simple(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: true
	// Report:
	// integration-tests [❌ 66.7% pass, 0.89 avg] (2/3)
	// └ 1 [FAIL] Timeout in payment service
}

// ExampleByEval demonstrates basic usage of the ByEval report generator.
func ExampleByEval() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add evaluation data following /{model}/{test case}/{eval} pattern
	// Model: claude, Test case: security-check, Eval: no-vulnerabilities
	evalObs1 := obs.Child("claude").Child("security-check").Child("no-vulnerabilities")
	evalObs1.Fail("Buffer overflow detected")
	evalObs1.Increment()
	evalObs1.Increment()

	// Model: gemini, Test case: security-check, Eval: no-vulnerabilities
	evalObs2 := obs.Child("gemini").Child("security-check").Child("no-vulnerabilities")
	evalObs2.Increment()

	// Generate ByEval report with 80% threshold
	reportStr, hasFailures := report.ByEval(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: true
	// Report:
	// ## Summary Table
	//
	// | Evaluation Metric    | claude        | gemini      | Average  |
	// |----------------------|---------------|-------------|----------|
	// | no-vulnerabilities   | ❌ 50.0%      | 100.0%      | ❌ 75.0% |
	// |    └─ security-check | ❌ 0.50 (50%) | 1.00 (100%) | ❌ 75.0% |
	//
	// no-vulnerabilities [❌ 66.7%] (2/3)
	// └ claude [❌ 50.0%] (1/2)
	//   └ security-check [❌ 50.0%] (1/2)
	//     └ 1 [FAIL] Buffer overflow detected
}

// ExampleByEval_withGrades demonstrates ByEval with graded evaluations.
func ExampleByEval_withGrades() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add evaluation data with grades
	// Model: claude, Test case: code-quality, Eval: readability-score
	evalObs1 := obs.Child("claude").Child("code-quality").Child("readability-score")
	evalObs1.Grade(0.75, "Some improvements needed")
	evalObs1.Increment()

	// Model: gemini, Test case: code-quality, Eval: readability-score
	evalObs2 := obs.Child("gemini").Child("code-quality").Child("readability-score")
	evalObs2.Grade(0.90, "Very readable code")
	evalObs2.Increment()

	// Generate report with 80% threshold
	reportStr, hasFailures := report.ByEval(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: true
	// Report:
	// ## Summary Table
	//
	// | Evaluation Metric  | claude        | gemini     | Average |
	// |--------------------|---------------|------------|---------|
	// | readability-score  | ❌ 75.0%      | 90.0%      | 82.5%   |
	// |    └─ code-quality | ❌ 0.75 (75%) | 0.90 (90%) | 82.5%   |
	//
	// readability-score [0.82 avg] (2 results)
	// └ claude [❌ 0.75 avg] (1 result)
	//   └ code-quality [❌ 0.75 avg] (1 result)
	//     └ 1 [0.75] Some improvements needed
}

// ExampleByEval_multipleEvaluations demonstrates multiple evaluations organized by eval type.
func ExampleByEval_multipleEvaluations() {
	// Create a factory for result collectors
	factory := func(name string) *evals.ResultCollector {
		baseObs := &exampleObserver{name: name}
		return evals.NewResultCollector(baseObs)
	}

	// Create root observer
	obs := evals.NewNamespacedObserver(factory)

	// Add data for multiple evaluations
	// Security evaluation - passing
	securityObs := obs.Child("claude").Child("auth-test").Child("security-check")
	securityObs.Increment()

	// Performance evaluation - passing
	perfObs := obs.Child("claude").Child("load-test").Child("performance-check")
	perfObs.Grade(0.85, "Good performance")
	perfObs.Increment()

	// Performance evaluation - failing (below 80% threshold)
	perfFailObs := obs.Child("claude").Child("stress-test").Child("performance-check")
	perfFailObs.Grade(0.65, "Performance issues under load")
	perfFailObs.Increment()

	// Generate report
	reportStr, hasFailures := report.ByEval(obs, 0.8)

	fmt.Printf("Has failures: %t\n", hasFailures)
	fmt.Printf("Report:\n%s", reportStr)

	// Output:
	// Has failures: true
	// Report:
	// ## Summary Table
	//
	// | Evaluation Metric | claude         | Average  |
	// |-------------------|----------------|----------|
	// | performance-check | ❌ 2/2 (75.0%) | ❌ 75.0% |
	// |    └─ stress-test | ❌ 0.65 (65%)  | ❌ 65.0% |
	// | security-check    | 100.0%         | 100.0%   |
	//
	// performance-check [❌ 0.75 avg] (2 results)
	// └ claude [❌ 0.75 avg] (2 results)
	//   └ stress-test [❌ 0.65 avg] (1 result)
	//     └ 1 [0.65] Performance issues under load
	// security-check [100.0%] (1/1)
}
