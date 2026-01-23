/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package report provides report generation functionality for evaluation results.

# Overview

The report package offers different report generators that can process
NamespacedObserver trees containing ResultCollector data to produce
markdown-formatted evaluation reports.

# Generator Types

All generators implement the Generator function type:

	type Generator func(obs *evals.NamespacedObserver[*evals.ResultCollector], threshold float64) (string, bool)

Available generators:

  - Simple: Hierarchical report following namespace structure, showing pass rates, grades, and failures
  - ByEval: Report organized by evaluation type, then by model, then by test case (requires /{model}/{test case}/{eval} path structure)

# Usage

	import "chainguard.dev/driftlessaf/agents/evals/report"

	// Create some evaluation data
	obs := evals.NewNamespacedObserver(func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(customObserver(name))
	})

	// Generate a simple hierarchical report
	reportStr, hasFailures := report.Simple(obs, 0.8)
	if hasFailures {
		fmt.Printf("Report:\n%s", reportStr)
	}

	// Generate a report organized by evaluation type
	reportStr, hasFailures = report.ByEval(obs, 0.8)
	if hasFailures {
		fmt.Printf("Report:\n%s", reportStr)
	}

# Report Format

Reports are generated in markdown format with:
  - Hierarchical headers based on namespace depth
  - Pass rates and average grades
  - Failure message lists
  - Below-threshold grade details

# Thread Safety

All generators are safe for concurrent use as they are pure functions that do not
modify their input parameters. Multiple goroutines can safely call any generator
function simultaneously with the same or different observers.
*/
package report
