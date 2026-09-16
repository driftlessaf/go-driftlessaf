/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
)

// Sequence returns an Analyzer that runs analyzers one after another over the
// same paths and prior diagnostics and concatenates their diagnostics in order.
// The first error stops the sequence and is returned with the failing
// analyzer's type. Use it to place several reviewers behind one [SubmitGate]:
// result validators run in parallel with each other and the leased worktree is
// not safe for concurrent use, so reviewers must not be registered as separate
// gates.
func Sequence(analyzers ...Analyzer) Analyzer {
	return sequence(analyzers)
}

type sequence []Analyzer

var _ Analyzer = sequence(nil)

// Analyze implements Analyzer.
func (s sequence) Analyze(ctx context.Context, wt *gogit.Worktree, paths []string, prior ...Diagnostic) ([]Diagnostic, error) {
	var out []Diagnostic
	for _, a := range s {
		diags, err := a.Analyze(ctx, wt, paths, prior...)
		if err != nil {
			return nil, fmt.Errorf("analyzer %T: %w", a, err)
		}
		out = append(out, diags...)
	}
	return out, nil
}
