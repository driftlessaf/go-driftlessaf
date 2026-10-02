/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"fmt"
	"strconv"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-github/v88/github"
)

// CheckProvider handles the CI check findings of one CI system: where their
// logs come from and how their checks are rerun. See WithCheckProviders.
type CheckProvider interface {
	// Match reports whether the provider owns the finding.
	Match(f callbacks.Finding) bool

	// Logs fetches the finding's log. gh, owner, and repo are the session's
	// client and repository.
	Logs(ctx context.Context, gh *github.Client, owner, repo string, f callbacks.Finding) (string, error)

	// Rerun reruns the finding's check.
	Rerun(ctx context.Context, gh *github.Client, owner, repo string, f callbacks.Finding) error
}

// WithCheckProviders adds providers for CI check findings. They are tried in
// order, before the built-in GitHub Actions provider, and the first match
// handles the finding. A finding no provider matches serves its Details as
// its log and is rerun by re-requesting its check run.
func WithCheckProviders[T any](providers ...CheckProvider) Option[T] {
	return func(cm *CM[T]) {
		cm.checkProviders = append(cm.checkProviders, providers...)
	}
}

// matchCheckProvider returns the first of providers that matches f, or
// checkRun when none does.
func matchCheckProvider(providers []CheckProvider, f callbacks.Finding) CheckProvider {
	for _, p := range providers {
		if p.Match(f) {
			return p
		}
	}
	return checkRun{}
}

// checkRun handles a check run no provider matches, using only what GitHub
// holds for any check: its logs are the finding's Details, and a rerun
// re-requests the check run, as the GitHub UI "Re-run" button does for
// non-Actions checks.
type checkRun struct{}

var _ CheckProvider = (*checkRun)(nil)

// Match implements CheckProvider.
func (checkRun) Match(callbacks.Finding) bool { return true }

// Logs implements CheckProvider.
func (checkRun) Logs(_ context.Context, _ *github.Client, _, _ string, f callbacks.Finding) (string, error) {
	return f.Details, nil
}

// Rerun implements CheckProvider.
func (checkRun) Rerun(ctx context.Context, gh *github.Client, owner, repo string, f callbacks.Finding) error {
	checkRunID, err := strconv.ParseInt(f.Identifier, 10, 64)
	if err != nil {
		return fmt.Errorf("parse check run ID from identifier %q: %w", f.Identifier, err)
	}
	if _, err := gh.Checks.ReRequestCheckRun(ctx, owner, repo, checkRunID); err != nil {
		return fmt.Errorf("re-request check run %d: %w", checkRunID, err)
	}
	return nil
}
