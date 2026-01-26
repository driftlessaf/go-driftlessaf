/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package report

import (
	"chainguard.dev/driftlessaf/agents/evals"
)

// Generator is a function type that generates reports from a NamespacedObserver tree.
// It takes an observer tree and a threshold, returning a report string and a boolean
// indicating if any evaluations fell below the threshold.
type Generator func(obs *evals.NamespacedObserver[*evals.ResultCollector], threshold float64) (string, bool)
