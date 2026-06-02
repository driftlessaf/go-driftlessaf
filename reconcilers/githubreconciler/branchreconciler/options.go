/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package branchreconciler

import (
	"errors"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
)

// Option configures a Reconciler.
type Option func(*Reconciler)

// WithBranchNamer sets the function to convert keys to branch names.
func WithBranchNamer(f BranchNamer) Option {
	return func(r *Reconciler) {
		r.branchNamer = f
	}
}

// WithAgentFunc sets the agent execution function.
func WithAgentFunc(f AgentFunc) Option {
	return func(r *Reconciler) {
		r.agentFunc = f
	}
}

// WithCriteriaFunc sets the acceptance criteria evaluation function.
func WithCriteriaFunc(f CriteriaFunc) Option {
	return func(r *Reconciler) {
		r.criteriaFunc = f
	}
}

// WithOnSuccess sets the success callback function executed when criteria is met.
// This is optional - if not provided, reconciliation completes without additional actions.
// Common use cases: trigger workflows, create PRs, send notifications, deploy changes.
func WithOnSuccess(f SuccessFunc) Option {
	return func(r *Reconciler) {
		r.onSuccess = f
	}
}

// WithBaseBranch sets the base branch (default: "main").
func WithBaseBranch(branch string) Option {
	return func(r *Reconciler) {
		r.baseBranch = branch
	}
}

// WithMaxAttempts sets the maximum reconciliation attempts (via commit count).
func WithMaxAttempts(n int) Option {
	return func(r *Reconciler) {
		r.maxAttempts = n
	}
}

// WithRequeueDelay sets the delay between reconciliation attempts.
func WithRequeueDelay(d time.Duration) Option {
	return func(r *Reconciler) {
		r.requeueDelay = d
	}
}

// New creates a new branch reconciler.
func New(
	cloneMeta *clonemanager.Meta,
	clientCache *githubreconciler.ClientCache,
	opts ...Option,
) (*Reconciler, error) {
	r := &Reconciler{
		cloneMeta:    cloneMeta,
		clientCache:  clientCache,
		baseBranch:   "main",
		maxAttempts:  10,
		requeueDelay: 5 * time.Minute,
	}

	for _, opt := range opts {
		opt(r)
	}

	if r.branchNamer == nil {
		return nil, errors.New("branchNamer is required")
	}
	if r.agentFunc == nil {
		return nil, errors.New("agentFunc is required")
	}
	if r.criteriaFunc == nil {
		return nil, errors.New("criteriaFunc is required")
	}

	return r, nil
}
