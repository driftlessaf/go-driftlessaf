/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
)

// core holds the state shared by every reconciler variant in this package
// and implements the pull request reconciliation flow (see pr.go), which is
// common to all variants. Variants embed core and add their own path leg.
type core struct {
	identity      string
	analyzer      Analyzer
	statusManager *statusmanager.StatusManager[CheckDetails]
	cloneMeta     *clonemanager.Meta
	mode          Mode

	// labels are static labels applied by the variant's path leg.
	// See WithLabels.
	labels []string

	// labelFn optionally computes additional labels from diagnostics/findings.
	// See WithLabelFunc.
	labelFn func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string
}

// newCore builds the shared reconciler state from the common configuration.
func newCore(ctx context.Context, identity string, analyzer Analyzer, cloneMeta *clonemanager.Meta, o commonOptions) (core, error) {
	sm, err := statusmanager.NewStatusManager[CheckDetails](ctx, identity)
	if err != nil {
		return core{}, fmt.Errorf("create status manager: %w", err)
	}
	return core{
		identity:      identity,
		analyzer:      analyzer,
		statusManager: sm,
		cloneMeta:     cloneMeta,
		mode:          o.mode,
		labels:        o.labels,
		labelFn:       o.labelFn,
	}, nil
}
