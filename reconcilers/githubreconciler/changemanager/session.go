/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package changemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v84/github"
	"github.com/shurcooL/githubv4"
)

// State is a bit-field representing the composite state of a PR.
// Multiple flags can be set simultaneously (e.g., a PR can need a rebase
// and have findings and have pending checks all at once).
type State int

const (
	// StateNoPR indicates no existing PR.
	StateNoPR State = 1 << iota
	// StateNeedsRebase indicates the PR has merge conflicts.
	StateNeedsRebase
	// StateUnknown indicates GitHub is still computing mergeability.
	StateUnknown
	// StateHasFindings indicates the PR has CI failures to address.
	// Only set when WithFindingsIteration is enabled.
	StateHasFindings
	// StatePending indicates CI checks are still running.
	StatePending
	// StateMaxCommits indicates the PR has reached the maximum number of commits.
	StateMaxCommits
)

// HasPR returns true if a PR exists.
func (s State) HasPR() bool { return s&StateNoPR == 0 }

// NeedsRebase returns true if the PR has merge conflicts.
func (s State) NeedsRebase() bool { return s&StateNeedsRebase != 0 }

// IsUnknown returns true if GitHub is still computing mergeability.
func (s State) IsUnknown() bool { return s&StateUnknown != 0 }

// HasFindings returns true if the PR has CI failures to address.
func (s State) HasFindings() bool { return s&StateHasFindings != 0 }

// HasPendingChecks returns true if CI checks are still running.
func (s State) HasPendingChecks() bool { return s&StatePending != 0 }

// HitMaxCommits returns true if the PR has reached the maximum commit limit.
func (s State) HitMaxCommits() bool { return s&StateMaxCommits != 0 }

// HasNoConflicts returns true if the PR exists, has no merge conflicts,
// and mergeability is known.
func (s State) HasNoConflicts() bool {
	return s.HasPR() && !s.NeedsRebase() && !s.IsUnknown()
}

// Session represents work on a specific PR for a specific resource.
type Session[T any] struct {
	manager    *CM[T]
	client     *github.Client
	gqlClient  *graphqlclient.GraphQLClient
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
	prAssignees []string // Login names of PR assignees

	commitCount   int                 // Total number of commits on the PR
	findings      []callbacks.Finding // CI failures detected on the existing PR
	pendingChecks []string            // Names of checks that are not yet complete
}

// skipLabel returns the skip label for this session's identity.
func (s *Session[T]) skipLabel() string {
	return "skip:" + s.manager.identity
}

// ShouldSkip checks if the existing PR should be skipped.
// Returns true if the PR has a skip label or is assigned to someone not in
// excludeAssignees. Assignees listed in excludeAssignees (e.g. the issue
// creator, assigned by the bot) are excluded from the check; only assignees
// outside this list indicate that a human has taken over the PR.
// Returns false if no existing PR exists.
func (s *Session[T]) ShouldSkip(excludeAssignees ...string) bool {
	if s.prNumber == 0 {
		return false
	}
	if slices.Contains(s.prLabels, s.skipLabel()) {
		return true
	}
	// Only skip if there are assignees that are not in the exclude list.
	excluded := make(map[string]struct{}, len(excludeAssignees))
	for _, a := range excludeAssignees {
		excluded[a] = struct{}{}
	}
	for _, a := range s.prAssignees {
		if _, ok := excluded[a]; !ok {
			return true
		}
	}
	return false
}

// HasSkipLabel returns true if the PR has the skip label applied.
// Unlike ShouldSkip, this does not consider assignees.
func (s *Session[T]) HasSkipLabel() bool {
	return s.prNumber != 0 && slices.Contains(s.prLabels, s.skipLabel())
}

// HasLabel returns true if the PR has the specified label.
func (s *Session[T]) HasLabel(labelName string) bool {
	return s.prNumber != 0 && slices.Contains(s.prLabels, labelName)
}

// State returns the composite state of the PR as a bit-field.
// Multiple flags can be set simultaneously.
func (s *Session[T]) State() State {
	if s.prNumber == 0 {
		return StateNoPR
	}
	var state State
	switch {
	case s.prMergeable == nil:
		state |= StateUnknown
	case !*s.prMergeable:
		state |= StateNeedsRebase
	}
	if s.manager.maxCommits > 0 && s.commitCount >= s.manager.maxCommits {
		state |= StateMaxCommits
	}
	if s.manager.handlesFindings && len(s.findings) > 0 {
		state |= StateHasFindings
	}
	if len(s.pendingChecks) > 0 {
		state |= StatePending
	}
	return state
}

// CommitCount returns the number of commits on the PR.
// Returns 0 if no PR exists.
func (s *Session[T]) CommitCount() int {
	return s.commitCount
}

// PendingChecks returns the names of checks that are not yet complete.
func (s *Session[T]) PendingChecks() []string {
	return s.pendingChecks
}

// PRNumber returns the number of the existing PR, or 0 if none exists.
func (s *Session[T]) PRNumber() int {
	return s.prNumber
}

// Assignees returns the login names of users assigned to the existing PR.
func (s *Session[T]) Assignees() []string {
	return s.prAssignees
}

// Labels returns the label names on the existing PR.
func (s *Session[T]) Labels() []string {
	return s.prLabels
}

// Extract returns the embedded data from the PR body.
func (s *Session[T]) Extract() (*T, error) {
	if !s.State().HasPR() {
		return nil, nil
	}

	return s.manager.templateExecutor.Extract(s.prBody)
}

// ApplyTurnLimit adds a turn-limit label to the PR, preventing further
// commits from being added. Unlike adding a skip label, this does not
// block the PR from being rebased if it develops merge conflicts.
// Returns the PR URL. This is a no-op if no PR exists.
func (s *Session[T]) ApplyTurnLimit(ctx context.Context) (string, error) {
	if s.prNumber == 0 {
		return "", nil
	}
	turnLimitLabel := s.manager.identity + "/turn-limit"
	if slices.Contains(s.prLabels, turnLimitLabel) {
		clog.InfoContext(ctx, "PR already has turn-limit label", "pr", s.prNumber)
		return s.prURL, nil
	}
	clog.InfoContext(ctx, "PR hit turn limit, adding turn-limit label", "pr", s.prNumber, "commits", s.commitCount, "max", s.manager.maxCommits)

	if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, []string{turnLimitLabel}); err != nil {
		return "", fmt.Errorf("adding turn-limit label: %w", err)
	}
	return s.prURL, nil
}

// CloseAnyOutstanding closes the existing PR if one exists.
// If message is non-empty, it posts the message as a comment before closing.
// This is a no-op if no PR exists.
func (s *Session[T]) CloseAnyOutstanding(ctx context.Context, message string) error {
	if s.prNumber == 0 {
		return nil
	}

	clog.InfoContextf(ctx, "Closing PR #%d", s.prNumber)

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

// AddAssignees adds the given logins as assignees on the existing PR.
// This is a no-op if no PR exists or if all provided logins are already assigned.
func (s *Session[T]) AddAssignees(ctx context.Context, logins []string) error {
	if s.prNumber == 0 || len(logins) == 0 {
		return nil
	}
	// Filter out logins that are already assigned.
	existing := make(map[string]struct{}, len(s.prAssignees))
	for _, a := range s.prAssignees {
		existing[a] = struct{}{}
	}
	var toAdd []string
	for _, l := range logins {
		if _, ok := existing[l]; !ok {
			toAdd = append(toAdd, l)
		}
	}
	if len(toAdd) == 0 {
		return nil
	}
	if _, _, err := s.client.Issues.AddAssignees(ctx, s.owner, s.repo, s.prNumber, toAdd); err != nil {
		return fmt.Errorf("adding assignees: %w", err)
	}
	// Update the cached assignees so subsequent calls are accurate.
	s.prAssignees = append(s.prAssignees, toAdd...)
	return nil
}

// Findings returns the list of findings to be addressed.
// Returns nil if no PR exists or if all checks passed.
func (s *Session[T]) Findings() []callbacks.Finding {
	return s.findings
}

// FindingCallbacks returns callbacks for fetching finding details.
// The returned callbacks can be embedded into agent tool callbacks.
// Since all details are pre-fetched in NewSession, this just does a lookup.
func (s *Session[T]) FindingCallbacks() callbacks.FindingCallbacks {
	return callbacks.FindingCallbacks{
		Findings: s.findings,
		GetDetails: func(_ context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			for _, f := range s.findings {
				if f.Kind == kind && f.Identifier == identifier {
					return f.Details, nil
				}
			}
			return "", fmt.Errorf("finding not found: %s/%s", kind, identifier)
		},
		GetLogs: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			for _, f := range s.findings {
				if f.Kind == kind && f.Identifier == identifier {
					return fetchFindingLogs(ctx, s.client, s.owner, s.repo, f)
				}
			}
			return "", fmt.Errorf("finding not found: %s/%s", kind, identifier)
		},
		Retry: func(ctx context.Context, kind callbacks.FindingKind, identifier string) error {
			if kind != callbacks.FindingKindCICheck {
				return fmt.Errorf("retry is only supported for CI check findings, got: %s", kind)
			}
			for _, f := range s.findings {
				if f.Kind == kind && f.Identifier == identifier {
					return rerunCICheck(ctx, s.client, s.owner, s.repo, f.DetailsURL)
				}
			}
			return fmt.Errorf("finding not found: %s/%s", kind, identifier)
		},
		Resolve: func(ctx context.Context, identifier string) error {
			if strings.HasPrefix(identifier, reviewBodyIdentifierPrefix) {
				return errors.New("cannot resolve review body findings, only review thread findings can be resolved")
			}
			return resolveReviewThread(ctx, s.gqlClient, identifier)
		},
	}
}

// resolveReviewThread calls the GitHub resolveReviewThread GraphQL mutation.
func resolveReviewThread(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, threadID string) error {
	var mutation struct {
		ResolveReviewThread struct {
			Thread struct {
				Id         string
				IsResolved bool
			}
		} `graphql:"resolveReviewThread(input: $input)"`
	}

	return gqlClient.Mutate(ctx, "ResolveReviewThread", &mutation, githubv4.ResolveReviewThreadInput{
		ThreadID: githubv4.ID(threadID),
	}, nil)
}

// ErrNoChanges can be returned by the makeChanges callback to signal that no
// diff was produced. Upsert passes this error through (wrapped) so the caller
// can decide how to handle it (e.g. close an existing PR, log, or ignore).
var ErrNoChanges = errors.New("no changes")

// Upsert creates a new PR or updates an existing one with the provided properties.
// It only calls makeChanges when refresh is needed: no existing PR, merge conflict,
// CI failures (only when WithFindingsIteration is enabled), or embedded data differs.
//
// If makeChanges returns ErrNoChanges, it is passed through (wrapped) so the
// caller can check for it with errors.Is.
//
// Returns a RequeueAfter error if GitHub is still computing the PR's mergeable status.
// Returns an error if the PR should be skipped (skip label or assigned to someone).
func (s *Session[T]) Upsert(
	ctx context.Context,
	data *T,
	draft bool,
	labels []string,
	makeChanges func(ctx context.Context, branchName string) error,
) (prURL string, err error) {
	// Check if refresh is needed
	needsRefresh, err := s.needsRefresh(ctx, data, labels)
	if err != nil {
		return "", err
	}

	if !needsRefresh {
		clog.InfoContext(ctx, "PR is up to date, no refresh needed")
		return s.prURL, nil
	}

	// Make code changes on the branch
	if err := makeChanges(ctx, s.branchName); errors.Is(err, ErrNoChanges) {
		return "", fmt.Errorf("upsert %s: %w", s.branchName, err)
	} else if err != nil {
		return "", fmt.Errorf("making changes: %w", err)
	}

	// Compare the branch to base to check if the PR has any aggregate diff.
	// If the branch is identical to base after changes, close any existing PR
	// or skip creation entirely.
	comp, _, err := s.client.Repositories.CompareCommits(ctx, s.owner, s.repo, s.ref, s.branchName, &github.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("comparing branch to base: %w", err)
	}
	if len(comp.Files) == 0 {
		clog.InfoContextf(ctx, "Branch %s has no aggregate diff against %s", s.branchName, s.ref)
		return "", s.CloseAnyOutstanding(ctx, "Closing PR because all changes have been resolved.")
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
		clog.InfoContextf(ctx, "Creating new PR with head %s and base %s", s.branchName, s.ref)

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

		s.prNumber = pr.GetNumber()
		s.prURL = pr.GetHTMLURL()

		clog.InfoContextf(ctx, "Created PR #%d: %s", s.prNumber, s.prURL)
		return s.prURL, nil
	}

	// Update existing PR
	clog.InfoContextf(ctx, "Updating existing PR #%d", s.prNumber)

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

	clog.InfoContextf(ctx, "Updated PR #%d: %s", s.prNumber, s.prURL)
	return s.prURL, nil
}

// needsRefresh determines if an existing PR needs to be refreshed.
// Checks embedded data first, then falls through to mergeability and CI state.
func (s *Session[T]) needsRefresh(ctx context.Context, expected *T, desiredLabels []string) (bool, error) {
	state := s.State()

	if !state.HasPR() {
		return true, nil
	}

	// Check if embedded data differs before consulting mergeable state.
	// Compare via JSON round-trip so that fields tagged json:"-" (such as
	// Request, which is only used for template rendering) are excluded from
	// the comparison.
	existing, err := s.manager.templateExecutor.Extract(s.prBody)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to extract data from PR body: %v", err)
		return true, nil
	}

	existingJSON, err := json.Marshal(existing)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to marshal existing data: %v", err)
		return true, nil
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		clog.WarnContextf(ctx, "Failed to marshal expected data: %v", err)
		return true, nil
	}
	if !bytes.Equal(existingJSON, expectedJSON) {
		clog.InfoContextf(ctx, "PR data differs, refresh needed: existing=%s expected=%s", existingJSON, expectedJSON)
		return true, nil
	}

	// Data matches — now check mergeability and CI state.
	switch {
	case state.NeedsRebase(), state.HasFindings():
		return true, nil
	case state.IsUnknown():
		clog.InfoContext(ctx, "PR mergeable status is still being computed by GitHub, requeueing")
		return false, workqueue.RequeueAfter(30 * time.Second)
	}

	// Check if the PR is missing any desired labels.
	existingSet := make(map[string]struct{}, len(s.prLabels))
	for _, l := range s.prLabels {
		existingSet[l] = struct{}{}
	}
	for _, l := range desiredLabels {
		if _, ok := existingSet[l]; !ok {
			clog.InfoContextf(ctx, "PR missing desired label %q, refresh needed: existing=%v desired=%v", l, s.prLabels, desiredLabels)
			return true, nil
		}
	}

	return false, nil
}
