/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
)

// Option configures behavior common to every reconciler constructor in this
// package. It is accepted anywhere a constructor-specific option is.
type Option interface {
	PROption
}

// PROption configures a reconciler built with NewPR.
type PROption interface {
	applyPR(*prOptions)
}

// commonOptions holds the configuration shared by every reconciler variant.
type commonOptions struct {
	mode    Mode
	labels  []string
	labelFn func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string
}

// prOptions holds the configuration for a reconciler built with NewPR.
type prOptions struct {
	commonOptions
	baseRevalidation                bool
	unknownMergeabilityRequeueAfter time.Duration
	giveUp                          *changemanager.GiveUpComment
}

// option implements Option by mutating the common configuration.
type option func(*commonOptions)

func (f option) applyPR(o *prOptions) { f(&o.commonOptions) }

// prOption implements PROption.
type prOption func(*prOptions)

func (f prOption) applyPR(o *prOptions) { f(o) }

// WithMode configures the reconciler's operating mode.
func WithMode(m Mode) Option {
	return option(func(o *commonOptions) {
		o.mode = m
	})
}

// WithLabels appends static labels applied by the reconciler's path leg
// (for NewPR, the labels on the pull requests it creates). Labels
// accumulate across repeated uses.
func WithLabels(labels ...string) Option {
	return option(func(o *commonOptions) {
		o.labels = append(o.labels, labels...)
	})
}

// WithLabelFunc configures a function that computes additional labels
// based on analyzer diagnostics and/or CI findings. The returned labels
// are merged with the static labels configured via WithLabels.
//
// On the first pass (analyzer runs), diagnostics is populated and findings
// contains unfixed diagnostics converted to findings.
// On iteration passes (PR has CI failures), diagnostics is nil and findings
// contains the session's CI/review findings.
func WithLabelFunc(fn func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string) Option {
	return option(func(o *commonOptions) {
		o.labelFn = fn
	})
}

// WithBaseRevalidation makes the reconciler re-run the analyzer against the
// base branch before iterating on a PR with CI findings, closing PRs whose
// update has already landed and refreshing PRs superseded by a newer version
// with the newest update (see needsRefresh). Off by default to avoid the
// extra analyzer runs for reconcilers that do not need it.
func WithBaseRevalidation() PROption {
	return prOption(func(o *prOptions) {
		o.baseRevalidation = true
	})
}

// WithRequeueOnUnknownMergeability requeues after the given delay when GitHub
// has not yet computed a PR's mergeability and there is nothing else to act on
// (no findings, no pending checks), instead of resetting the PR from the
// default branch. A non-positive delay disables it (the default).
func WithRequeueOnUnknownMergeability(after time.Duration) PROption {
	return prOption(func(o *prOptions) {
		o.unknownMergeabilityRequeueAfter = after
	})
}

// WithGiveUpComment surfaces an agent's deliberate no-op on the PR. When the
// agent runs but makes no file changes and its result implements
// changemanager.Explainer with a non-empty explanation, the reconciler upserts
// a single comment (identified by marker) whose body is render(explanation).
// Repeated identical give-ups rewrite nothing, and the comment is cleared when
// the PR recovers. Off by default: reconcilers that do not set this keep the
// silent no-change behavior.
func WithGiveUpComment(marker string, render func(explanation string) string) PROption {
	return prOption(func(o *prOptions) {
		o.giveUp = &changemanager.GiveUpComment{Marker: marker, Render: render}
	})
}
