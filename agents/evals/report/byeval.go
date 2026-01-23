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
	evalName        string
	totalFailures   int64
	totalGrades     int64
	totalIterations int64
	avgGrade        float64
	passRate        float64
	modelResults    map[string]*modelResult
}

// modelResult holds results for a specific model within an evaluation
type modelResult struct {
	modelName       string
	totalFailures   int64
	totalGrades     int64
	totalIterations int64
	avgGrade        float64
	passRate        float64
	testCaseResults map[string]*testCaseResult
}

// testCaseResult holds results for a specific test case within a model
type testCaseResult struct {
	testCaseName string
	failures     []string
	grades       []evals.Grade
	iterations   int64
	passRate     float64
	avgGrade     float64
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

		// Calculate and store test case metrics
		testCase := calculateTestCaseMetrics(testCaseName, failures, grades, iterations)

		// Store the result and aggregate upwards
		evalResults[evalName].modelResults[modelName].testCaseResults[testCaseName] = testCase
		aggregateToModelLevel(evalResults[evalName].modelResults[modelName], failures, grades, iterations)
		aggregateToEvalLevel(evalResults[evalName], failures, grades, iterations)
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

// calculateTestCaseMetrics computes metrics for a single test case
func calculateTestCaseMetrics(testCaseName string, failures []string, grades []evals.Grade, iterations int64) *testCaseResult {
	passCount := iterations - int64(len(failures))
	passRate := float64(passCount) / float64(iterations)

	var avgGrade float64
	if len(grades) > 0 {
		var totalScore float64
		for _, grade := range grades {
			totalScore += grade.Score
		}
		avgGrade = totalScore / float64(len(grades))
	}

	return &testCaseResult{
		testCaseName: testCaseName,
		failures:     failures,
		grades:       grades,
		iterations:   iterations,
		passRate:     passRate,
		avgGrade:     avgGrade,
	}
}

// aggregateToModelLevel adds test case metrics to model totals
func aggregateToModelLevel(modelResult *modelResult, failures []string, grades []evals.Grade, iterations int64) {
	modelResult.totalFailures += int64(len(failures))
	modelResult.totalGrades += int64(len(grades))
	modelResult.totalIterations += iterations
}

// aggregateToEvalLevel adds test case metrics to evaluation totals
func aggregateToEvalLevel(evalResult *evalResult, failures []string, grades []evals.Grade, iterations int64) {
	evalResult.totalFailures += int64(len(failures))
	evalResult.totalGrades += int64(len(grades))
	evalResult.totalIterations += iterations
}

// calculateAggregatedMetrics computes pass rates and average grades for models and evaluations
func calculateAggregatedMetrics(evalResults map[string]*evalResult) {
	for _, evalResult := range evalResults {
		// Calculate eval-level aggregates
		if evalResult.totalIterations > 0 {
			evalResult.passRate = float64(evalResult.totalIterations-evalResult.totalFailures) / float64(evalResult.totalIterations)
		}
		if evalResult.totalGrades > 0 {
			evalResult.avgGrade = calculateTotalGradeScore(evalResult) / float64(evalResult.totalGrades)
		}

		// Calculate model-level aggregates
		for _, modelResult := range evalResult.modelResults {
			if modelResult.totalIterations > 0 {
				modelResult.passRate = float64(modelResult.totalIterations-modelResult.totalFailures) / float64(modelResult.totalIterations)
			}
			if modelResult.totalGrades > 0 {
				modelResult.avgGrade = calculateModelTotalGradeScore(modelResult) / float64(modelResult.totalGrades)
			}
		}
	}
}

// calculateTotalGradeScore sums all grade scores across all models and test cases
func calculateTotalGradeScore(evalResult *evalResult) float64 {
	var totalScore float64
	for _, modelResult := range evalResult.modelResults {
		totalScore += calculateModelTotalGradeScore(modelResult)
	}
	return totalScore
}

// calculateModelTotalGradeScore sums all grade scores for a specific model
func calculateModelTotalGradeScore(modelResult *modelResult) float64 {
	var totalScore float64
	for _, testCaseResult := range modelResult.testCaseResults {
		for _, grade := range testCaseResult.grades {
			totalScore += grade.Score
		}
	}
	return totalScore
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
		evalResult := evalResults[evalName]

		// Check if eval is below threshold
		if checkEvalBelowThreshold(evalResult, threshold) {
			hasFailure = true
		}

		// Add evaluation to tree and process models
		if addEvalToTree(tree, evalResult, threshold) {
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
	evalValue, evalLabel := formatEvalForTree(evalResult)

	// Add failure indicator if below threshold
	if checkEvalBelowThreshold(evalResult, threshold) {
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

// formatEvalForTree formats evaluation metrics for tree display
func formatEvalForTree(evalResult *evalResult) (string, string) {
	hasPassRate := evalResult.totalFailures > 0
	hasGrades := evalResult.totalGrades > 0

	switch {
	case hasPassRate && hasGrades:
		value := fmt.Sprintf("%.1f%% pass, %.2f avg", evalResult.passRate*100, evalResult.avgGrade)
		label := fmt.Sprintf("(%d/%d)", evalResult.totalIterations-evalResult.totalFailures, evalResult.totalIterations)
		return value, label
	case hasGrades:
		gradeWord := "results"
		if evalResult.totalGrades == 1 {
			gradeWord = "result"
		}
		value := fmt.Sprintf("%.2f avg", evalResult.avgGrade)
		label := fmt.Sprintf("(%d %s)", evalResult.totalGrades, gradeWord)
		return value, label
	default:
		value := fmt.Sprintf("%.1f%%", evalResult.passRate*100)
		label := fmt.Sprintf("(%d/%d)", evalResult.totalIterations-evalResult.totalFailures, evalResult.totalIterations)
		return value, label
	}
}

// addModelToTree adds model results to the tree under the evaluation
func addModelToTree(tree *pathtree.Tree, evalName string, modelResult *modelResult, threshold float64) bool {
	// Find failing test cases
	failingTestCases := findFailingTestCases(modelResult, threshold)

	// Format model header
	modelValue, modelLabel := formatModelForTree(modelResult)

	hasFailure := false
	// Add failure indicator if below threshold
	if checkModelBelowThreshold(modelResult, threshold) {
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
		testCaseValue, testCaseLabel := formatTestCaseForTree(testCaseResult)

		// Add failure indicator (all of these test cases are below threshold)
		testCaseValue = fmt.Sprintf("❌ %s", testCaseValue)

		// Add test case node
		testCasePath := fmt.Sprintf("%s/%s/%s", evalName, modelResult.modelName, testCaseName)
		_ = tree.Add(testCasePath, testCaseValue, testCaseLabel)

		// Add failure details as child nodes
		failureCount := addFailuresToTree(tree, testCasePath, testCaseResult.failures)
		_ = addBelowThresholdGradesToTree(tree, testCasePath, testCaseResult.grades, threshold, failureCount)
	}

	return hasFailure
}

// formatModelForTree formats model metrics for tree display
func formatModelForTree(modelResult *modelResult) (string, string) {
	hasPassRate := modelResult.totalFailures > 0
	hasGrades := modelResult.totalGrades > 0

	switch {
	case hasPassRate && hasGrades:
		value := fmt.Sprintf("%.1f%% pass, %.2f avg", modelResult.passRate*100, modelResult.avgGrade)
		label := fmt.Sprintf("(%d/%d)", modelResult.totalIterations-modelResult.totalFailures, modelResult.totalIterations)
		return value, label
	case hasGrades:
		gradeWord := "results"
		if modelResult.totalGrades == 1 {
			gradeWord = "result"
		}
		value := fmt.Sprintf("%.2f avg", modelResult.avgGrade)
		label := fmt.Sprintf("(%d %s)", modelResult.totalGrades, gradeWord)
		return value, label
	default:
		value := fmt.Sprintf("%.1f%%", modelResult.passRate*100)
		label := fmt.Sprintf("(%d/%d)", modelResult.totalIterations-modelResult.totalFailures, modelResult.totalIterations)
		return value, label
	}
}

// formatTestCaseForTree formats test case metrics for tree display
func formatTestCaseForTree(testCaseResult *testCaseResult) (string, string) {
	hasPassRate := len(testCaseResult.failures) > 0
	hasGrades := len(testCaseResult.grades) > 0

	switch {
	case hasPassRate && hasGrades:
		value := fmt.Sprintf("%.1f%% pass, %.2f avg", testCaseResult.passRate*100, testCaseResult.avgGrade)
		label := fmt.Sprintf("(%d/%d)", testCaseResult.iterations-int64(len(testCaseResult.failures)), testCaseResult.iterations)
		return value, label
	case hasGrades:
		gradeCount := len(testCaseResult.grades)
		gradeWord := "results"
		if gradeCount == 1 {
			gradeWord = "result"
		}
		value := fmt.Sprintf("%.2f avg", testCaseResult.avgGrade)
		label := fmt.Sprintf("(%d %s)", gradeCount, gradeWord)
		return value, label
	default:
		value := fmt.Sprintf("%.1f%%", testCaseResult.passRate*100)
		label := fmt.Sprintf("(%d/%d)", testCaseResult.iterations-int64(len(testCaseResult.failures)), testCaseResult.iterations)
		return value, label
	}
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
func addBelowThresholdGradesToTree(tree *pathtree.Tree, basePath string, grades []evals.Grade, threshold float64, startIndex int) int {
	count := 0
	for _, grade := range grades {
		if grade.Score < threshold {
			gradePath := fmt.Sprintf("%s/%d", basePath, startIndex+count+1)
			gradeValue := fmt.Sprintf("%.2f", grade.Score)
			_ = tree.Add(gradePath, gradeValue, grade.Reasoning)
			count++
		}
	}
	return count
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
				if modelResult.totalGrades > 0 {
					value = modelResult.avgGrade * 100
				}
				sum += value

				// Format with threshold check
				valueStr := fmt.Sprintf("%.1f%%", value)
				if len(modelResult.testCaseResults) > 1 {
					valueStr = fmt.Sprintf("%d/%d (%.1f%%)",
						modelResult.totalIterations-modelResult.totalFailures,
						modelResult.totalIterations, value)
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

// checkEvalBelowThreshold determines if an evaluation failed to meet the threshold
func checkEvalBelowThreshold(evalResult *evalResult, threshold float64) bool {
	if evalResult.passRate < threshold {
		return true
	}
	if evalResult.totalGrades > 0 && evalResult.avgGrade < threshold {
		return true
	}
	return false
}

// checkModelBelowThreshold determines if a model failed to meet the threshold
func checkModelBelowThreshold(modelResult *modelResult, threshold float64) bool {
	if modelResult.passRate < threshold {
		return true
	}
	if modelResult.totalGrades > 0 && modelResult.avgGrade < threshold {
		return true
	}
	return false
}

// findFailingTestCases identifies test cases that are below threshold
func findFailingTestCases(modelResult *modelResult, threshold float64) []string {
	var failingTestCases []string
	for testCaseName, testCaseResult := range modelResult.testCaseResults {
		if isTestCaseBelowThreshold(testCaseResult, threshold) {
			failingTestCases = append(failingTestCases, testCaseName)
		}
	}
	sort.Strings(failingTestCases)
	return failingTestCases
}

// isTestCaseBelowThreshold checks if a test case is below threshold
func isTestCaseBelowThreshold(testCaseResult *testCaseResult, threshold float64) bool {
	if testCaseResult.passRate < threshold {
		return true
	}
	if len(testCaseResult.grades) > 0 && testCaseResult.avgGrade < threshold {
		return true
	}
	return false
}
