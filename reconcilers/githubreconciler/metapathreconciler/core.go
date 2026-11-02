/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"slices"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/internal/commentack"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/statusmanager"
	"github.com/google/go-github/v88/github"
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

	// resourceLabelFn optionally computes labels from the resource alone.
	// See WithResourceLabels.
	resourceLabelFn func(*githubreconciler.Resource) []string

	// syntheticPaths, when true, means resource paths name no file on the
	// default branch, so a missing path is analyzed rather than treated as a
	// removed file. See WithSyntheticPaths.
	syntheticPaths bool

	// ackComments turns on the 👀 reaction on people's comments. See
	// WithHumanCommentAck.
	ackComments bool
	// commentAPI builds the client that reads comments and adds reactions. It is
	// a field so tests can replace it.
	commentAPI func(*github.Client) commentack.API
}

// staticLabels returns the labels a resource carries whatever the analysis
// finds: the WithLabels labels followed by the WithResourceLabels ones.
func (c *core) staticLabels(res *githubreconciler.Resource) []string {
	labels := slices.Clone(c.labels)
	if c.resourceLabelFn != nil {
		labels = append(labels, c.resourceLabelFn(res)...)
	}
	return labels
}

// newCore builds the shared reconciler state from the common configuration.
func newCore(ctx context.Context, identity string, analyzer Analyzer, cloneMeta *clonemanager.Meta, o commonOptions) (core, error) {
	sm, err := statusmanager.NewStatusManager[CheckDetails](ctx, identity)
	if err != nil {
		return core{}, fmt.Errorf("create status manager: %w", err)
	}
	return core{
		identity:        identity,
		analyzer:        analyzer,
		statusManager:   sm,
		cloneMeta:       cloneMeta,
		mode:            o.mode,
		labels:          o.labels,
		labelFn:         o.labelFn,
		resourceLabelFn: o.resourceLabelFn,
		syntheticPaths:  o.syntheticPaths,
		commentAPI:      func(gh *github.Client) commentack.API { return commentack.NewClient(gh) },
	}, nil
}
