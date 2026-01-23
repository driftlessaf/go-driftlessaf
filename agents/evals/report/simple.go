/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/sdk/pathtree"
)

// Simple walks a NamespacedObserver tree and generates a tree-based report
// showing pass rates, average grades, failures, and below-threshold grades.
// Returns the report string and a boolean indicating if any evaluations fell below the threshold.
func Simple(obs *evals.NamespacedObserver[*evals.ResultCollector], threshold float64) (string, bool) {
	tree := pathtree.New()
	tree.PrintOption = pathtree.KeyValueLabel
	hasFailure := false

	obs.Walk(func(name string, collector *evals.ResultCollector) {
		failures := collector.Failures()
		grades := collector.Grades()
		iterations := collector.Total()

		// Skip paths with no iterations
		if iterations == 0 {
			return
		}

		// Calculate pass rate based on failures
		passCount := iterations - int64(len(failures))
		passRate := float64(passCount) / float64(iterations)

		// Check if this evaluation is below threshold
		if passRate < threshold {
			hasFailure = true
		}

		// Calculate average grade if any grades exist
		var avgGrade float64
		var belowThresholdGrades []evals.Grade
		if len(grades) > 0 {
			var totalScore float64
			for _, grade := range grades {
				totalScore += grade.Score
				if grade.Score < threshold {
					belowThresholdGrades = append(belowThresholdGrades, grade)
				}
			}
			avgGrade = totalScore / float64(len(grades))

			// Check if average grade is below threshold
			if avgGrade < threshold {
				hasFailure = true
			}
		}

		// Format the value and label for the tree
		var value, label string

		// Determine what metrics to show
		hasPassRate := len(failures) > 0
		hasGrades := len(grades) > 0

		switch {
		case hasPassRate && hasGrades:
			// Show both pass rate and avg grade
			value = fmt.Sprintf("%.1f%% pass, %.2f avg", passRate*100, avgGrade)
			label = fmt.Sprintf("(%d/%d)", passCount, iterations)
		case hasGrades:
			// Show only avg grade
			gradeCount := len(grades)
			gradeWord := "results"
			if gradeCount == 1 {
				gradeWord = "result"
			}
			value = fmt.Sprintf("%.2f avg", avgGrade)
			label = fmt.Sprintf("(%d %s)", gradeCount, gradeWord)
		default:
			// Default to pass rate when no grades
			value = fmt.Sprintf("%.1f%%", passRate*100)
			label = fmt.Sprintf("(%d/%d)", passCount, iterations)
		}

		// Add failure indicators
		isBelowThreshold := passRate < threshold || (len(grades) > 0 && avgGrade < threshold)
		if isBelowThreshold {
			value = fmt.Sprintf("❌ %s", value)
		}

		// Add to tree
		err := tree.Add(name, value, label)
		if err != nil {
			// If there's a conflict, update instead
			_ = tree.Update(name, value, label)
		}

		// Add failure messages as child nodes in the tree
		if len(failures) > 0 {
			for i, failure := range failures {
				failurePath := fmt.Sprintf("%s/%d", name, i+1)
				_ = tree.Add(failurePath, "FAIL", failure)
			}
		}

		// Add below-threshold grades as child nodes in the tree
		if len(belowThresholdGrades) > 0 {
			for i, grade := range belowThresholdGrades {
				gradePath := fmt.Sprintf("%s/%d", name, len(failures)+i+1)
				gradeValue := fmt.Sprintf("%.2f", grade.Score)
				_ = tree.Add(gradePath, gradeValue, grade.Reasoning)
			}
		}
	})

	return tree.String(), hasFailure
}
