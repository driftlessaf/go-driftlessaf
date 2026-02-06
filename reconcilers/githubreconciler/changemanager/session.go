/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v75/github"
)

// Session represents work on a specific PR for a specific resource.
type Session[T any] struct {
	manager    *CM[T]
	client     *github.Client
	resource   *githubreconciler.Resource
	owner      string
	repo       string
	branchName string
	ref        string // Base branch for the PR

	// Existing PR state (populated by NewSession if a PR exists)
	prNumber    int      // 0 if no existing PR
	prURL       string   // HTML URL of existing PR
	prBody      string   // Body text of existing PR
	prMergeable *bool    // nil if GitHub is still computing
	prLabels    []string // Label names on existing PR

	findings      []toolcall.Finding // CI failures detected on the existing PR
	pendingChecks []string           // Names of checks that are not yet complete
}

// skipLabel returns the skip label for this session's identity.
func (s *Session[T]) skipLabel() string {
	return "skip:" + s.manager.identity
}

// HasSkipLabel checks if the existing PR has a skip label.
// Returns false if no existing PR exists.
func (s *Session[T]) HasSkipLabel() bool {
	if s.prNumber == 0 {
		return false
	}
	return slices.Contains(s.prLabels, s.skipLabel())
}

// HasPendingChecks returns true if there are checks that are not yet complete.
func (s *Session[T]) HasPendingChecks() bool {
	return len(s.pendingChecks) > 0
}

// PendingChecks returns the names of checks that are not yet complete.
func (s *Session[T]) PendingChecks() []string {
	return s.pendingChecks
}

// CloseAnyOutstanding closes the existing PR if one exists.
// If message is non-empty, it posts the message as a comment before closing.
// This is a no-op if no PR exists.
func (s *Session[T]) CloseAnyOutstanding(ctx context.Context, message string) error {
	if s.prNumber == 0 {
		return nil
	}

	log := clog.FromContext(ctx)
	log.Infof("Closing PR #%d", s.prNumber)

	// Post message as a comment if provided
	if message != "" {
		if _, _, err := s.client.Issues.CreateComment(ctx, s.owner, s.repo, s.prNumber, &github.IssueComment{
			Body: github.Ptr(message),
		}); err != nil {
			return fmt.Errorf("posting comment: %w", err)
		}
	}

	_, _, err := s.client.PullRequests.Edit(ctx, s.owner, s.repo, s.prNumber, &github.PullRequest{
		State: github.Ptr("closed"),
	})
	if err != nil {
		return fmt.Errorf("closing pull request: %w", err)
	}

	return nil
}

// HasFindings returns true if the existing PR has CI failures that need addressing.
// Returns false if no PR exists or if all checks passed.
func (s *Session[T]) HasFindings() bool {
	return len(s.findings) > 0
}

// Findings returns the list of findings to be addressed.
// Returns nil if no PR exists or if all checks passed.
func (s *Session[T]) Findings() []toolcall.Finding {
	return s.findings
}

// Upsert creates a new PR or updates an existing one with the provided properties.
// It only calls makeChanges when refresh is needed: no existing PR, merge conflict,
// CI failures (findings), or embedded data differs.
// Returns a RequeueAfter error if GitHub is still computing the PR's mergeable status.
// Returns an error if the skip label is present on the existing PR.
func (s *Session[T]) Upsert(
	ctx context.Context,
	data *T,
	draft bool,
	labels []string,
	makeChanges func(ctx context.Context, branchName string) error,
) (prURL string, err error) {
	log := clog.FromContext(ctx)

	// Check if refresh is needed
	needsRefresh, err := s.needsRefresh(ctx, data)
	if err != nil {
		return "", err
	}

	if !needsRefresh {
		log.Info("PR is up to date, no refresh needed")
		return s.prURL, nil
	}

	// Make code changes on the branch
	if err := makeChanges(ctx, s.branchName); err != nil {
		return "", fmt.Errorf("making changes: %w", err)
	}

	// Generate PR title and body from templates
	title, err := s.manager.templateExecutor.Execute(s.manager.titleTemplate, data)
	if err != nil {
		return "", fmt.Errorf("executing title template: %w", err)
	}

	body, err := s.manager.templateExecutor.Execute(s.manager.bodyTemplate, data)
	if err != nil {
		return "", fmt.Errorf("executing body template: %w", err)
	}

	body += fmt.Sprintf("\n\n> **Note:** If you need to make manual changes to this PR, apply the `skip:%s` label so the reconciler won't overwrite them.", s.manager.identity)

	// Embed data in body
	body, err = s.manager.templateExecutor.Embed(body, data)
	if err != nil {
		return "", fmt.Errorf("embedding data: %w", err)
	}

	if s.prNumber == 0 {
		// Create new PR
		log.Infof("Creating new PR with head %s and base %s", s.branchName, s.ref)

		pr, _, err := s.client.PullRequests.Create(ctx, s.owner, s.repo, &github.NewPullRequest{
			Title: github.Ptr(title),
			Body:  github.Ptr(body),
			Head:  github.Ptr(s.branchName),
			Base:  github.Ptr(s.ref),
			Draft: github.Ptr(draft),
		})
		if err != nil {
			return "", fmt.Errorf("creating pull request: %w", err)
		}

		// Apply labels
		if len(labels) > 0 {
			if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, pr.GetNumber(), labels); err != nil {
				return "", fmt.Errorf("adding labels: %w", err)
			}
		}

		log.Infof("Created PR #%d: %s", pr.GetNumber(), pr.GetHTMLURL())
		return pr.GetHTMLURL(), nil
	}

	// Update existing PR
	log.Infof("Updating existing PR #%d", s.prNumber)

	// Refetch PR to check for skip label (could have been added since session creation)
	freshPR, _, err := s.client.PullRequests.Get(ctx, s.owner, s.repo, s.prNumber)
	if err != nil {
		return "", fmt.Errorf("refetching pull request: %w", err)
	}

	// Check skip label on fresh PR
	skipLabel := s.skipLabel()
	for _, label := range freshPR.Labels {
		if label.GetName() == skipLabel {
			return "", errors.New("PR has skip label, not updating to avoid stomping manual changes")
		}
	}

	_, _, err = s.client.PullRequests.Edit(ctx, s.owner, s.repo, s.prNumber, &github.PullRequest{
		Title: github.Ptr(title),
		Body:  github.Ptr(body),
		Draft: github.Ptr(draft),
	})
	if err != nil {
		return "", fmt.Errorf("updating pull request: %w", err)
	}

	// Replace labels
	if _, _, err := s.client.Issues.ReplaceLabelsForIssue(ctx, s.owner, s.repo, s.prNumber, labels); err != nil {
		return "", fmt.Errorf("replacing labels: %w", err)
	}

	log.Infof("Updated PR #%d: %s", s.prNumber, s.prURL)
	return s.prURL, nil
}

// needsRefresh determines if an existing PR needs to be refreshed.
// Returns true if no existing PR, PR has merge conflict, or embedded data differs.
// Returns an error if the Mergeable status is still being computed by GitHub (RequeueAfter 5 minutes).
func (s *Session[T]) needsRefresh(ctx context.Context, expected *T) (bool, error) {
	if s.prNumber == 0 {
		return true, nil
	}

	log := clog.FromContext(ctx)

	// Check if GitHub is still computing the mergeable status
	// See: https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28#get-a-pull-request
	// "The value of the mergeable attribute can be true, false, or null. If the value is null,
	// then GitHub has started a background job to compute the mergeability."
	if s.prMergeable == nil {
		log.Info("PR mergeable status is still being computed by GitHub, requeueing")
		return false, workqueue.RequeueAfter(5 * time.Minute)
	}

	// Check for merge conflicts
	if !*s.prMergeable {
		log.Info("PR has merge conflict, refresh needed")
		return true, nil
	}

	// Check for CI failures that need addressing
	if len(s.findings) > 0 {
		log.Info("PR has CI failures, refresh needed")
		return true, nil
	}

	// Extract embedded data from PR body
	existing, err := s.manager.templateExecutor.Extract(s.prBody)
	if err != nil {
		log.Warnf("Failed to extract data from PR body: %v", err)
		return true, nil
	}

	// Compare data using deep equality
	if !reflect.DeepEqual(existing, expected) {
		log.Infof("PR data differs, refresh needed: %s", cmp.Diff(existing, expected))
		return true, nil
	}

	return false, nil
}
