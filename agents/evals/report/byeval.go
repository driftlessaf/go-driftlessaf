/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/sdk/pathtree"
	"github.com/olekukonko/tablewriter"
)

// ByEval generates a report organized by evaluation, then by model, then by test case.
// Assumes paths follow the pattern /{model}/{test case}/{eval} for paths with results.
// Returns the report string and a boolean indicating if any evaluations fell below the threshold.
func ByEval(obs *evals.NamespacedObserver[*evals.ResultCollector], threshold float64) (string, bool) {
	// Collect and organize all evaluation results
	evalResults := collectEvalResults(obs)

	// Calculate aggregated metrics
	calculateAggregatedMetrics(evalResults)

	// Generate the formatted report
	return generateFormattedReport(evalResults, threshold)
}

// evalResult holds aggregated results for a specific evaluation across models and test cases
type evalResult struct {
	evalName string
	stats
	modelResults map[string]*modelResult
}

// modelResult holds results for a specific model within an evaluation
type modelResult struct {
	modelName string
	stats
	testCaseResults map[string]*testCaseResult
}

// testCaseResult holds results for a specific test case within a model
type testCaseResult struct {
	testCaseName string
	failures     []string
	grades       []evals.Grade
	stats
}

// collectEvalResults walks the observer tree and organizes results by evaluation
func collectEvalResults(obs *evals.NamespacedObserver[*evals.ResultCollector]) map[string]*evalResult {
	evalResults := make(map[string]*evalResult)

	obs.Walk(func(name string, collector *evals.ResultCollector) {
		failures := collector.Failures()
		grades := collector.Grades()
		iterations := collector.Total()

		// Skip paths with no iterations
		if iterations == 0 {
			return
		}

		// Parse and validate path components
		modelName, testCaseName, evalName, ok := parsePath(name)
		if !ok {
			return
		}

		// Initialize nested structure if needed
		initializeEvalResult(evalResults, evalName, modelName)

		// Build the test case result
		testCase := &testCaseResult{
			testCaseName: testCaseName,
			failures:     failures,
			grades:       grades,
		}
		testCase.add(failures, grades, iterations)

		// Store the result and aggregate upwards
		evalResults[evalName].modelResults[modelName].testCaseResults[testCaseName] = testCase
		evalResults[evalName].modelResults[modelName].add(failures, grades, iterations)
		evalResults[evalName].add(failures, grades, iterations)
	})

	return evalResults
}

// parsePath extracts model, test case, and eval names from a path
func parsePath(name string) (modelName, testCaseName, evalName string, ok bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// initializeEvalResult ensures the nested structure exists for the given path
func initializeEvalResult(evalResults map[string]*evalResult, evalName, modelName string) {
	if evalResults[evalName] == nil {
		evalResults[evalName] = &evalResult{
			evalName:     evalName,
			modelResults: make(map[string]*modelResult),
		}
	}

	if evalResults[evalName].modelResults[modelName] == nil {
		evalResults[evalName].modelResults[modelName] = &modelResult{
			modelName:       modelName,
			testCaseResults: make(map[string]*testCaseResult),
		}
	}
}

// calculateAggregatedMetrics computes pass rates and average grades at every level
func calculateAggregatedMetrics(evalResults map[string]*evalResult) {
	for _, evalResult := range evalResults {
		evalResult.finalize()
		for _, modelResult := range evalResult.modelResults {
			modelResult.finalize()
			for _, testCaseResult := range modelResult.testCaseResults {
				testCaseResult.finalize()
			}
		}
	}
}

// generateFormattedReport creates the tree-based report from processed results
func generateFormattedReport(evalResults map[string]*evalResult, threshold float64) (string, bool) {
	var report strings.Builder
	hasFailure := false

	// Generate and add summary table
	summaryTable := generateSummaryTable(evalResults, threshold)
	if summaryTable != "" {
		report.WriteString(summaryTable)
		report.WriteString("\n")
	}

	// Create tree for hierarchical display
	tree := pathtree.New()
	tree.PrintOption = pathtree.KeyValueLabel

	// Sort evaluation names for consistent output
	evalNames := make([]string, 0, len(evalResults))
	for evalName := range evalResults {
		evalNames = append(evalNames, evalName)
	}
	sort.Strings(evalNames)

	for _, evalName := range evalNames {
		// Add evaluation to tree and process models
		if addEvalToTree(tree, evalResults[evalName], threshold) {
			hasFailure = true
		}
	}

	// Add tree output to report
	if len(evalNames) > 0 {
		report.WriteString(tree.String())
	}

	return report.String(), hasFailure
}

// addEvalToTree adds evaluation results to the tree structure
func addEvalToTree(tree *pathtree.Tree, evalResult *evalResult, threshold float64) bool {
	hasFailure := false

	// Format evaluation header
	evalValue, evalLabel := evalResult.formatForTree()

	// Add failure indicator if below threshold
	if evalResult.belowThreshold(threshold) {
		evalValue = fmt.Sprintf("❌ %s", evalValue)
		hasFailure = true
	}

	// Add evaluation node
	_ = tree.Add(evalResult.evalName, evalValue, evalLabel)

	// Sort models for consistent output
	modelNames := make([]string, 0, len(evalResult.modelResults))
	for modelName := range evalResult.modelResults {
		modelNames = append(modelNames, modelName)
	}
	sort.Strings(modelNames)

	for _, modelName := range modelNames {
		modelResult := evalResult.modelResults[modelName]

		// Add model to tree and process test cases
		if addModelToTree(tree, evalResult.evalName, modelResult, threshold) {
			hasFailure = true
		}
	}

	return hasFailure
}

// addModelToTree adds model results to the tree under the evaluation
func addModelToTree(tree *pathtree.Tree, evalName string, modelResult *modelResult, threshold float64) bool {
	// Find failing test cases
	failingTestCases := findFailingTestCases(modelResult, threshold)

	// Format model header
	modelValue, modelLabel := modelResult.formatForTree()

	hasFailure := false
	// Add failure indicator if below threshold
	if modelResult.belowThreshold(threshold) {
		modelValue = fmt.Sprintf("❌ %s", modelValue)
		hasFailure = true
	} else if len(failingTestCases) == 0 {
		return false
	}

	// Add model node
	modelPath := fmt.Sprintf("%s/%s", evalName, modelResult.modelName)
	_ = tree.Add(modelPath, modelValue, modelLabel)

	// Add failing test cases and their details
	for _, testCaseName := range failingTestCases {
		testCaseResult := modelResult.testCaseResults[testCaseName]

		// Format test case
		testCaseValue, testCaseLabel := testCaseResult.formatForTree()

		// Add failure indicator (all of these test cases are below threshold)
		testCaseValue = fmt.Sprintf("❌ %s", testCaseValue)

		// Add test case node
		testCasePath := fmt.Sprintf("%s/%s/%s", evalName, modelResult.modelName, testCaseName)
		_ = tree.Add(testCasePath, testCaseValue, testCaseLabel)

		// Add failure details as child nodes
		failureCount := addFailuresToTree(tree, testCasePath, testCaseResult.failures)
		addBelowThresholdGradesToTree(tree, testCasePath, testCaseResult.grades, threshold, failureCount)
	}

	return hasFailure
}

// addFailuresToTree adds failure messages as child nodes
func addFailuresToTree(tree *pathtree.Tree, basePath string, failures []string) int {
	for i, failure := range failures {
		failurePath := fmt.Sprintf("%s/%d", basePath, i+1)
		_ = tree.Add(failurePath, "FAIL", failure)
	}
	return len(failures)
}

// addBelowThresholdGradesToTree adds below-threshold grades as child nodes
func addBelowThresholdGradesToTree(tree *pathtree.Tree, basePath string, grades []evals.Grade, threshold float64, startIndex int) {
	count := 0
	for _, grade := range grades {
		if grade.Score < threshold {
			gradePath := fmt.Sprintf("%s/%d", basePath, startIndex+count+1)
			gradeValue := fmt.Sprintf("%.2f", grade.Score)
			_ = tree.Add(gradePath, gradeValue, grade.Reasoning)
			count++
		}
	}
}

// generateSummaryTable creates a hierarchical summary table showing model performance across evaluations
func generateSummaryTable(evalResults map[string]*evalResult, threshold float64) string {
	// If no evaluations exist, return empty string
	if len(evalResults) == 0 {
		return ""
	}

	// Collect all model names
	modelSet := make(map[string]struct{})
	for _, evalResult := range evalResults {
		for modelName := range evalResult.modelResults {
			modelSet[modelName] = struct{}{}
		}
	}

	// Convert to sorted slice
	modelNames := make([]string, 0, len(modelSet))
	for modelName := range modelSet {
		modelNames = append(modelNames, modelName)
	}
	sort.Strings(modelNames)

	// If no models found, return empty string
	if len(modelNames) == 0 {
		return ""
	}

	// Create table headers
	headers := append([]string{"Evaluation Metric"}, modelNames...)
	headers = append(headers, "Average")

	// Use a buffer to capture table output
	var buf bytes.Buffer
	table := createStandardTable(headers, &buf)

	// Get sorted evaluation names for consistent output
	evalNames := make([]string, 0, len(evalResults))
	for evalName := range evalResults {
		evalNames = append(evalNames, evalName)
	}
	sort.Strings(evalNames)

	// Process each evaluation
	for _, evalName := range evalNames {
		evalResult := evalResults[evalName]
		if evalResult == nil {
			continue
		}

		// Build evaluation row
		row := []string{evalName}
		var sum float64

		for _, modelName := range modelNames {
			modelResult := evalResult.modelResults[modelName]
			if modelResult != nil {
				value := modelResult.passRate * 100
				if modelResult.gradeCount > 0 {
					value = modelResult.avgGrade * 100
				}
				sum += value

				// Format with threshold check
				valueStr := fmt.Sprintf("%.1f%%", value)
				if len(modelResult.testCaseResults) > 1 {
					valueStr = fmt.Sprintf("%d/%d (%.1f%%)",
						modelResult.iterations-modelResult.failureCount,
						modelResult.iterations, value)
				}

				if value < threshold*100 {
					row = append(row, fmt.Sprintf("❌ %s", valueStr))
				} else {
					row = append(row, valueStr)
				}
			} else {
				row = append(row, "-")
				// Missing model contributes 0 to sum
			}
		}

		// Calculate and add average
		var avg float64
		if len(modelNames) > 0 {
			avg = sum / float64(len(modelNames))
		}

		if avg < threshold*100 {
			row = append(row, fmt.Sprintf("❌ %.1f%%", avg))
		} else {
			row = append(row, fmt.Sprintf("%.1f%%", avg))
		}

		_ = table.Append(row)

		// Add test case rows with indentation
		addTestCaseSummaryRows(table, evalResult, modelNames, threshold)
	}

	_ = table.Render()

	// Return table with header
	return fmt.Sprintf("## Summary Table\n\n%s", buf.String())
}

// addTestCaseSummaryRows adds the nested test case rows for an evaluation to the table
func addTestCaseSummaryRows(table *tablewriter.Table, evalResult *evalResult, modelNames []string, threshold float64) {
	// Collect all test case names
	testCaseSet := make(map[string]struct{})
	for _, modelResult := range evalResult.modelResults {
		for testCaseName := range modelResult.testCaseResults {
			testCaseSet[testCaseName] = struct{}{}
		}
	}

	// Convert to sorted slice
	testCaseNames := make([]string, 0, len(testCaseSet))
	for testCaseName := range testCaseSet {
		testCaseNames = append(testCaseNames, testCaseName)
	}
	sort.Strings(testCaseNames)

	// Build rows and only append those with entries below threshold
	var validRows [][]string
	for _, testCaseName := range testCaseNames {
		row := []string{testCaseName}
		var sum float64
		hasBelowThreshold := false

		for _, modelName := range modelNames {
			modelResult := evalResult.modelResults[modelName]
			if modelResult != nil {
				testCaseResult := modelResult.testCaseResults[testCaseName]
				if testCaseResult != nil {
					value := testCaseResult.passRate * 100
					if len(testCaseResult.grades) > 0 {
						value = testCaseResult.avgGrade * 100
					}
					sum += value

					valueStr := fmt.Sprintf("%.2f (%.0f%%)", value/100, value)
					if value < threshold*100 {
						row = append(row, fmt.Sprintf("❌ %s", valueStr))
						hasBelowThreshold = true
					} else {
						row = append(row, valueStr)
					}
				} else {
					row = append(row, "-")
				}
			} else {
				row = append(row, "-")
			}
		}

		// Calculate and add average
		var avg float64
		if len(modelNames) > 0 {
			avg = sum / float64(len(modelNames))
		}

		if avg < threshold*100 {
			row = append(row, fmt.Sprintf("❌ %.1f%%", avg))
			hasBelowThreshold = true
		} else {
			row = append(row, fmt.Sprintf("%.1f%%", avg))
		}

		// Only include rows that have at least one entry below threshold
		if hasBelowThreshold {
			validRows = append(validRows, row)
		}
	}

	// Add valid rows with proper tree prefixes
	for i, row := range validRows {
		if i == len(validRows)-1 {
			row[0] = fmt.Sprintf("   └─ %s", row[0])
		} else {
			row[0] = fmt.Sprintf("   ├─ %s", row[0])
		}
		_ = table.Append(row)
	}
}

// findFailingTestCases identifies test cases that are below threshold
func findFailingTestCases(modelResult *modelResult, threshold float64) []string {
	var failingTestCases []string
	for testCaseName, testCaseResult := range modelResult.testCaseResults {
		if testCaseResult.belowThreshold(threshold) {
			failingTestCases = append(failingTestCases, testCaseName)
		}
	}
	sort.Strings(failingTestCases)
	return failingTestCases
}
