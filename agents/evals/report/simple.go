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

		// Aggregate metrics for this path
		var pathStats stats
		pathStats.add(failures, grades, iterations)
		pathStats.finalize()

		// Format the value and label for the tree
		value, label := pathStats.formatForTree()

		// Add failure indicators
		if pathStats.belowThreshold(threshold) {
			hasFailure = true
			value = fmt.Sprintf("❌ %s", value)
		}

		// Add to tree
		if err := tree.Add(name, value, label); err != nil {
			// If there's a conflict, update instead
			_ = tree.Update(name, value, label)
		}

		// Add failure messages and below-threshold grades as child nodes in the tree
		failureCount := addFailuresToTree(tree, name, failures)
		addBelowThresholdGradesToTree(tree, name, grades, threshold, failureCount)
	})

	return tree.String(), hasFailure
}
