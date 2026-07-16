/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package report provides report generation functionality for evaluation results.

# Overview

The report package processes NamespacedObserver trees containing
ResultCollector data to produce markdown-formatted evaluation reports.

ByEval generates a report organized by evaluation type, then by model, then by
test case. It requires observer paths following the /{model}/{test case}/{eval}
structure.

# Usage

	import "chainguard.dev/driftlessaf/agents/evals/report"

	// Create some evaluation data
	obs := evals.NewNamespacedObserver(func(name string) *evals.ResultCollector {
		return evals.NewResultCollector(customObserver(name))
	})

	// Generate a report organized by evaluation type
	reportStr, hasFailures := report.ByEval(obs, 0.8)
	if hasFailures {
		fmt.Printf("Report:\n%s", reportStr)
	}

# Report Format

Reports are generated in markdown format with:
  - A summary table of pass rates and grades per evaluation and model
  - Hierarchical detail trees per evaluation
  - Failure message lists
  - Below-threshold grade details

# Thread Safety

ByEval is safe for concurrent use as it is a pure function that does not
modify its input parameters. Multiple goroutines can safely call it
simultaneously with the same or different observers.
*/
package report
