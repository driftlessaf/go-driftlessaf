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
	"net/http"
	"slices"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/graphqlclient"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v88/github"
	"github.com/shurcooL/githubv4"
	"go.opentelemetry.io/otel/trace"
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
	prHeadSHA   string   // Head commit SHA of existing PR
	prMergeable *bool    // nil if GitHub is still computing
	prDraft     bool     // whether the existing PR is a draft
	prLabels    []string // Label names on existing PR
	prAssignees []string // Login names of PR assignees

	commitCount   int                 // Total number of commits on the PR
	findings      []callbacks.Finding // CI failures detected on the existing PR
	pendingChecks []string            // Names of checks that are not yet complete
	meta          metadata            // Changemanager state embedded in the PR body

	// reviewThreadsAwaitingReply holds the node ids of unresolved review threads
	// whose most recent relevant comment is not this bot's own reply, so the bot
	// still owes a response. HasUnresolvedReviews consults it so a thread the bot
	// has already answered does not renew the commit budget.
	reviewThreadsAwaitingReply map[string]struct{}
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

// IssueHasSkipLabel returns true if issue carries this identity's skip label.
// It is the issue-scoped counterpart to ShouldSkip's PR check: a reconciler can
// leave an issue (and any PR) alone via the same skip:<identity> label, read
// from the already-fetched issue.
func (s *Session[T]) IssueHasSkipLabel(issue *github.Issue) bool {
	skip := s.skipLabel()
	for _, l := range issue.Labels {
		if l.GetName() == skip {
			return true
		}
	}
	return false
}

// HasLabel returns true if the PR has the specified label.
func (s *Session[T]) HasLabel(labelName string) bool {
	return s.prNumber != 0 && slices.Contains(s.prLabels, normalizeLabel(labelName))
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
	if s.manager.maxCommits > 0 && s.commitBudgetUsed() >= s.manager.maxCommits {
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

// commitBudgetUsed returns the commit count the turn limit is measured against:
// commits since the last ResetCommitBudget under WithDynamicCommitBudget
// (NewSession clamps the baseline, so never negative), else the total count.
func (s *Session[T]) commitBudgetUsed() int {
	if !s.manager.dynamicCommitBudget {
		return s.commitCount
	}
	return s.commitCount - s.meta.CommitBudgetBaseline
}

// PendingChecks returns the names of checks that are not yet complete.
func (s *Session[T]) PendingChecks() []string {
	return s.pendingChecks
}

// PRNumber returns the number of the existing PR, or 0 if none exists.
func (s *Session[T]) PRNumber() int {
	return s.prNumber
}

// HeadSHA returns the head commit SHA of the existing PR, or "" if none exists.
func (s *Session[T]) HeadSHA() string {
	return s.prHeadSHA
}

// IsDraft reports whether the existing PR is a draft. False when no PR exists.
func (s *Session[T]) IsDraft() bool {
	return s.prDraft
}

// Assignees returns the login names of users assigned to the existing PR.
func (s *Session[T]) Assignees() []string {
	return s.prAssignees
}

// Labels returns the label names on the existing PR.
func (s *Session[T]) Labels() []string {
	return s.prLabels
}

// Path returns the resource path being reconciled (e.g. "packages/foo.yaml").
func (s *Session[T]) Path() string {
	return s.resource.Path
}

// Extract returns the embedded data from the PR body.
func (s *Session[T]) Extract() (*T, error) {
	if !s.State().HasPR() {
		return nil, nil
	}

	return s.manager.Extract(s.prBody)
}

// State-label suffixes are defined once so readers and writers cannot drift.
const (
	turnLimitLabelSuffix      = "/turn-limit"
	readyForReviewLabelSuffix = "/ready-for-review"
	gaveUpLabelSuffix         = "/too-hard-need-human"
)

func (s *Session[T]) turnLimitLabel() string {
	return normalizeLabel(s.manager.identity + turnLimitLabelSuffix)
}

func (s *Session[T]) readyForReviewLabel() string {
	return normalizeLabel(s.manager.identity + readyForReviewLabelSuffix)
}

func (s *Session[T]) gaveUpLabel() string {
	return normalizeLabel(s.manager.identity + gaveUpLabelSuffix)
}

// HasTurnLimitLabel reports whether the PR carries this identity's
// turn-limit label (applied by ApplyTurnLimit). False when no PR exists.
func (s *Session[T]) HasTurnLimitLabel() bool {
	return s.HasLabel(s.turnLimitLabel())
}

// HasReadyForReviewLabel reports whether the PR carries this identity's
// ready-for-review label (applied by ApplyReadyForReview). False when no PR
// exists.
func (s *Session[T]) HasReadyForReviewLabel() bool {
	return s.HasLabel(s.readyForReviewLabel())
}

// HasGaveUpLabel reports whether the PR carries this identity's
// too-hard-need-human label (applied by ApplyGaveUp, removed by
// ClearGaveUp). False when no PR exists.
func (s *Session[T]) HasGaveUpLabel() bool {
	return s.HasLabel(s.gaveUpLabel())
}

// PRURL returns the HTML URL of the existing PR, or "" if none exists.
func (s *Session[T]) PRURL() string {
	return s.prURL
}

// ApplyTurnLimit adds a turn-limit label to the PR, preventing further
// commits from being added. Unlike adding a skip label, this does not
// block the PR from being rebased if it develops merge conflicts.
// Returns the PR URL. This is a no-op if no PR exists.
func (s *Session[T]) ApplyTurnLimit(ctx context.Context) (string, error) {
	if s.prNumber == 0 {
		return "", nil
	}
	turnLimitLabel := s.turnLimitLabel()
	if slices.Contains(s.prLabels, turnLimitLabel) {
		clog.InfoContext(ctx, "PR already has turn-limit label", "pr", s.prNumber)
		return s.prURL, nil
	}
	clog.InfoContext(ctx, "PR hit turn limit, adding turn-limit label", "pr", s.prNumber, "commits", s.commitCount, "max", s.manager.maxCommits)

	if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, []string{turnLimitLabel}); err != nil {
		return "", fmt.Errorf("adding turn-limit label: %w", err)
	}
	// Cache the label so a same-session caller sees it.
	s.prLabels = append(s.prLabels, turnLimitLabel)
	return s.prURL, nil
}

// ResetCommitBudget gives the PR a fresh WithMaxCommits-sized budget by moving
// the baseline to the current commit count. The change is in memory and persisted
// by the next Upsert, so reset and then iterate. No-op without a PR or
// WithDynamicCommitBudget.
//
// Gate the reset on review state that clears once addressed (e.g.
// HasUnresolvedReviews): if the gating signal never clears, the budget renews
// every round and maxCommits becomes a per-turn rather than total limit.
//
// A per-PR cap (WithMaxBudgetResets, default 3) bounds how many times the budget
// renews, so even a review signal that never clears cannot extend the turn limit
// without bound. The reset count is persisted in the PR body, so it holds across
// reconciles. Once the cap is reached the reset is a logged no-op.
//
// The cap applies to an effective reset count, not to the stored value alone:
// the count persisted in the body is combined with a lower bound the commit
// history implies (with WithMaxCommits set to N, a PR with C commits proves at
// least floor((C-1)/N) resets), and the larger governs. Editing the body to
// lower the count therefore cannot renew the budget more than one extra time
// beyond what the commits already prove, and adding commits only tightens the
// bound. Negative stored counts are treated as zero. The effective value is
// persisted, so a lowered count is corrected on the next renewal.
func (s *Session[T]) ResetCommitBudget(ctx context.Context) {
	if s.prNumber == 0 || !s.manager.dynamicCommitBudget {
		return
	}
	if s.meta.CommitBudgetBaseline == s.commitCount {
		return
	}
	limit := s.manager.maxBudgetResets
	if limit <= 0 {
		limit = defaultMaxBudgetResets
	}
	resets := s.effectiveBudgetResets()
	if resets >= limit {
		clog.InfoContext(ctx, "Commit budget reset cap reached, not renewing", "pr", s.prNumber, "resets", resets, "cap", limit)
		return
	}
	s.meta.BudgetResetCount = resets + 1
	clog.InfoContext(ctx, "Resetting dynamic commit budget", "pr", s.prNumber, "baseline", s.commitCount, "previous", s.meta.CommitBudgetBaseline, "resets", s.meta.BudgetResetCount, "cap", limit)
	s.meta.CommitBudgetBaseline = s.commitCount
}

// effectiveBudgetResets returns the reset count the WithMaxBudgetResets cap is
// enforced against. It never trusts the PR body alone: the stored count
// (negative values treated as zero) is combined with a lower bound the commit
// history implies. With WithMaxCommits set to N, each budget admits at most N
// commits, so a PR with C commits has already consumed at least floor((C-1)/N)
// resets. The larger of the stored and derived counts is returned, so a body
// edited to lower the stored count cannot claim fewer resets than the commits
// prove, and adding commits only raises the derived bound. With no commit limit
// configured (WithMaxCommits unset, so N == 0) the derived bound is zero and
// only the stored count applies.
func (s *Session[T]) effectiveBudgetResets() int {
	stored := max(s.meta.BudgetResetCount, 0)
	derived := 0
	if s.manager.maxCommits > 0 && s.commitCount > 0 {
		derived = (s.commitCount - 1) / s.manager.maxCommits
	}
	return max(stored, derived)
}

// maxReasoningEntries caps the reasoning log persisted in the PR body so the
// body stays bounded; AppendReasoning drops the oldest entries beyond it.
// Generous relative to commit budgets (WithMaxCommits deployments run around
// 10), so in practice nothing is dropped.
const maxReasoningEntries = 20

// AppendReasoning records the agent's reasoning summary for the commit
// identified by commitHeadline (the commit message's first line). The entry
// joins the reasoning log persisted in the PR body by the next Upsert, so the
// log accumulates one entry per commit across iterations. A no-op when
// summary is empty (a run that carried no reasoning contributes no entry).
// Once the log holds maxReasoningEntries entries, the oldest are dropped.
//
// Call this only when a commit is actually being created (not on the
// ErrNoChanges path): only then does Upsert regenerate the body that
// persists the log.
func (s *Session[T]) AppendReasoning(commitHeadline, summary string) {
	if summary == "" {
		return
	}
	s.meta.ReasoningLog = append(s.meta.ReasoningLog, ReasoningEntry{
		CommitHeadline: commitHeadline,
		Summary:        summary,
	})
	if len(s.meta.ReasoningLog) > maxReasoningEntries {
		s.meta.ReasoningLog = s.meta.ReasoningLog[len(s.meta.ReasoningLog)-maxReasoningEntries:]
	}
}

// ReasoningLog returns the per-commit reasoning entries recovered from the PR
// body plus any appended in this session, oldest first. Callers render it
// into PR-body fields (e.g. a per-commit "Agent reasoning" section). The
// returned slice is the session's own; treat it as read-only.
func (s *Session[T]) ReasoningLog() []ReasoningEntry {
	return s.meta.ReasoningLog
}

// ApplyReadyForReview adds a ready-for-review label to the PR, signaling
// that the bot has stopped iterating because CI is green and human review
// is needed. Idempotent: re-running when the label is already present is
// a no-op. This is a no-op if no PR exists. Returns the PR URL.
func (s *Session[T]) ApplyReadyForReview(ctx context.Context) (string, error) {
	if s.prNumber == 0 {
		return "", nil
	}
	label := s.readyForReviewLabel()
	if slices.Contains(s.prLabels, label) {
		return s.prURL, nil
	}
	clog.InfoContext(ctx, "PR is green, adding ready-for-review label", "pr", s.prNumber)
	if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, []string{label}); err != nil {
		return "", fmt.Errorf("adding ready-for-review label: %w", err)
	}
	// Cache the label so a same-session caller sees it.
	s.prLabels = append(s.prLabels, label)
	return s.prURL, nil
}

// ApplyGaveUp adds an <identity>/too-hard-need-human label to the PR,
// signaling that the agent has posted a give-up explanation and is handing
// the PR off for human review. Idempotent: re-running when the label is
// already present is a no-op. This is a no-op if no PR exists. Returns the
// PR URL.
func (s *Session[T]) ApplyGaveUp(ctx context.Context) (string, error) {
	if s.prNumber == 0 {
		return "", nil
	}
	label := s.gaveUpLabel()
	if slices.Contains(s.prLabels, label) {
		return s.prURL, nil
	}
	clog.InfoContext(ctx, "Agent gave up, adding too-hard-need-human label", "pr", s.prNumber, "label", label)
	if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, []string{label}); err != nil {
		return "", fmt.Errorf("adding too-hard-need-human label: %w", err)
	}
	// Cache the label so a same-session caller sees it.
	s.prLabels = append(s.prLabels, label)
	return s.prURL, nil
}

// ClearGaveUp removes the <identity>/too-hard-need-human label, signaling
// that the agent has recovered (pushed a fix, or the PR otherwise resolved).
// Pairs with ApplyGaveUp so the label tracks the give-up comment lifecycle.
// No-op if the label is not present on the PR or no PR exists. Returns the
// PR URL.
func (s *Session[T]) ClearGaveUp(ctx context.Context) (string, error) {
	if s.prNumber == 0 {
		return "", nil
	}
	label := s.gaveUpLabel()
	if !slices.Contains(s.prLabels, label) {
		return s.prURL, nil
	}
	clog.InfoContext(ctx, "Agent recovered, removing too-hard-need-human label", "pr", s.prNumber, "label", label)
	if _, err := s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, s.prNumber, label); err != nil {
		return "", fmt.Errorf("removing too-hard-need-human label: %w", err)
	}
	s.prLabels = slices.DeleteFunc(s.prLabels, func(l string) bool { return l == label })
	return s.prURL, nil
}

// AddLabels adds the given labels to the existing PR. Labels exceeding
// GitHub's limit are shortened to a 40-character prefix and 10-character hash.
// This is a no-op if no PR exists or if all provided labels are already present.
func (s *Session[T]) AddLabels(ctx context.Context, labels []string) error {
	if s.prNumber == 0 || len(labels) == 0 {
		return nil
	}
	labels = normalizeLabels(labels)
	// Filter out labels that are already present.
	existing := make(map[string]struct{}, len(s.prLabels))
	for _, l := range s.prLabels {
		existing[l] = struct{}{}
	}
	var toAdd []string
	for _, l := range labels {
		if _, ok := existing[l]; !ok {
			toAdd = append(toAdd, l)
		}
	}
	if len(toAdd) == 0 {
		return nil
	}
	if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, toAdd); err != nil {
		return fmt.Errorf("adding labels: %w", err)
	}
	// Update the cached labels so subsequent calls are accurate.
	s.prLabels = append(s.prLabels, toAdd...)
	return nil
}

// ReplaceLabels adds the given labels to the existing PR (like AddLabels) and
// then, for each prefix in prefixes, removes labels under that prefix that are
// not in labels. Membership is per set, not one label per prefix: when labels
// contains several entries under one prefix, all of them stay. Labels under
// prefixes not listed in prefixes are never touched. With empty prefixes this
// is equivalent to AddLabels.
//
// The add happens before the remove so that a failure between the two steps
// leaves the PR with a duplicate label (repaired on the next reconcile) rather
// than with no label at all.
//
// A prefix with no desired label under it is skipped entirely: the caller's
// label derivation can legitimately produce nothing for a key on a given
// round, and that absence must not prune labels applied earlier.
// A failed removal does not stop the remaining removals, and the joined
// error reports every failure.
// This is a no-op if no PR exists.
func (s *Session[T]) ReplaceLabels(ctx context.Context, labels []string, prefixes []string) error {
	if s.prNumber == 0 {
		return nil
	}
	if err := s.AddLabels(ctx, labels); err != nil {
		return err
	}
	desired := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		desired[l] = struct{}{}
	}
	var errs []error
	for _, prefix := range prefixes {
		if !slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, prefix) }) {
			continue
		}
		var stale []string
		for _, l := range s.prLabels {
			if !strings.HasPrefix(l, prefix) {
				continue
			}
			if _, ok := desired[l]; !ok {
				stale = append(stale, l)
			}
		}
		for _, l := range stale {
			clog.InfoContext(ctx, "Removing stale label", "pr", s.prNumber, "label", l)
			if _, err := s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, s.prNumber, l); err != nil {
				// Keep the label cached: it is still on the PR.
				errs = append(errs, fmt.Errorf("removing label %q: %w", l, err))
				continue
			}
			s.prLabels = slices.DeleteFunc(s.prLabels, func(cached string) bool { return cached == l })
		}
	}
	return errors.Join(errs...)
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
			Body: new(message),
		}); err != nil {
			return fmt.Errorf("posting comment: %w", err)
		}
	}

	_, _, err := s.client.PullRequests.Edit(ctx, s.owner, s.repo, s.prNumber, &github.PullRequest{
		State: new("closed"),
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

// UpsertMarkerComment posts or updates a single PR comment identified by a
// hidden HTML marker, so repeated calls update the same comment in place rather
// than accumulating duplicates. The marker is prepended to body and used to
// locate the existing comment on subsequent calls. When a comment with the
// marker already exists and its body is unchanged, no API write is made — this
// keeps a recurring signal (e.g. an agent reporting it has nothing to fix)
// quiet across reconcile loops. This is a no-op if no PR exists.
//
// The find-then-write is not atomic; its dedup relies on reconciles being
// serialized per resource (as the workqueue guarantees). A caller composing
// this outside that guarantee could race two creates into duplicate comments.
func (s *Session[T]) UpsertMarkerComment(ctx context.Context, marker, body string) error {
	if s.prNumber == 0 {
		return nil
	}

	want := marker + "\n" + body
	_, err := s.upsertMarkerCommentOn(ctx, s.prNumber, marker, want, func(existing string) bool { return existing == want })
	return err
}

// UpsertIssueMarkerComment posts or updates a single comment on the source
// issue, identified by a hidden HTML marker, so repeated calls update the same
// comment in place rather than accumulating duplicates. Unlike
// UpsertMarkerComment it targets the issue (s.resource.Number) and is not gated
// on a PR existing, so a reconciler can announce work on the issue before the
// first PR appears. It is a no-op on non-issue resources.
func (s *Session[T]) UpsertIssueMarkerComment(ctx context.Context, marker, body string) error {
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return nil
	}
	want := marker + "\n" + body
	_, err := s.upsertMarkerCommentOn(ctx, s.resource.Number, marker, want, func(existing string) bool { return existing == want })
	return err
}

// revisionMarker renders the hidden line that stamps a marker comment with the
// revision of its subject (see UpsertIssueMarkerCommentForRevision).
func revisionMarker(revision string) string {
	return "<!--revision:" + revision + "-->"
}

// UpsertIssueMarkerCommentForRevision is UpsertIssueMarkerComment for a body
// that varies run to run while its subject does not — an agent's prose
// explanation of the same issue, say. revision identifies the subject (e.g. a
// hash of the issue body) and is stamped into the comment as a hidden line
// after the marker. An existing comment carrying the same revision is left
// untouched even when body differs; only a changed revision rewrites it. This
// matters on issues in particular: an issue comment re-triggers the reconcile
// that posted it, so a comment rewritten on every retry would loop the
// reconciler on its own output. It reports whether the comment was created or
// rewritten (false on a dedup, a tolerated permission error, or a non-issue
// resource).
func (s *Session[T]) UpsertIssueMarkerCommentForRevision(ctx context.Context, marker, revision, body string) (bool, error) {
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return false, nil
	}
	prefix := marker + "\n" + revisionMarker(revision) + "\n"
	return s.upsertMarkerCommentOn(ctx, s.resource.Number, marker, prefix+body, func(existing string) bool {
		return strings.HasPrefix(existing, prefix)
	})
}

// upsertMarkerCommentOn is the find-or-create body shared by the marker
// comment upserts. number is the issue/PR number to comment on, want the full
// comment body to write (marker included), and upToDate decides whether an
// existing marker comment already says what want says, in which case no API
// write is made. It reports whether a comment was created or edited.
func (s *Session[T]) upsertMarkerCommentOn(ctx context.Context, number int, marker, want string, upToDate func(existing string) bool) (bool, error) {
	existing, err := s.findMarkerComment(ctx, number, marker)
	// ferr (not err) so the classified list error is what we return without
	// shadowing the err reused by the edit/create calls below.
	if done, ferr := s.skipMarkerCommentIfForbidden(ctx, number, "listing comments", err); done {
		return false, ferr
	}
	if existing != nil {
		if upToDate(existing.GetBody()) {
			clog.InfoContextf(ctx, "Marker comment already up to date on #%d, skipping", number)
			return false, nil
		}
		clog.InfoContextf(ctx, "Updating marker comment on #%d", number)
		_, _, err = s.client.Issues.EditComment(ctx, s.owner, s.repo, existing.GetID(), &github.IssueComment{
			Body: new(want),
		})
		_, cerr := s.skipMarkerCommentIfForbidden(ctx, number, "editing marker comment", err)
		return err == nil, cerr
	}

	clog.InfoContextf(ctx, "Posting marker comment on #%d", number)
	_, _, err = s.client.Issues.CreateComment(ctx, s.owner, s.repo, number, &github.IssueComment{
		Body: new(want),
	})
	_, cerr := s.skipMarkerCommentIfForbidden(ctx, number, "posting marker comment", err)
	return err == nil, cerr
}

// DeleteMarkerComment removes the comment identified by marker, if present. It
// is the inverse of UpsertMarkerComment: callers delete the comment once the
// condition it described no longer holds (e.g. the agent recovered and pushed a
// fix). This is a no-op if no PR exists or no matching comment is found.
func (s *Session[T]) DeleteMarkerComment(ctx context.Context, marker string) error {
	if s.prNumber == 0 {
		return nil
	}
	return s.deleteMarkerCommentOn(ctx, s.prNumber, marker)
}

// DeleteIssueMarkerComment removes the comment identified by marker from the
// source issue, if present. It is the inverse of UpsertIssueMarkerComment and
// UpsertIssueMarkerCommentForRevision. This is a no-op on non-issue resources
// or when no matching comment is found.
func (s *Session[T]) DeleteIssueMarkerComment(ctx context.Context, marker string) error {
	if s.resource == nil || s.resource.Type != githubreconciler.ResourceTypeIssue {
		return nil
	}
	return s.deleteMarkerCommentOn(ctx, s.resource.Number, marker)
}

// deleteMarkerCommentOn is the find-and-delete body shared by
// DeleteMarkerComment (targeting the PR) and DeleteIssueMarkerComment
// (targeting the issue).
func (s *Session[T]) deleteMarkerCommentOn(ctx context.Context, number int, marker string) error {
	existing, err := s.findMarkerComment(ctx, number, marker)
	if done, ferr := s.skipMarkerCommentIfForbidden(ctx, number, "listing comments", err); done {
		return ferr
	}
	if existing == nil {
		return nil
	}

	clog.InfoContextf(ctx, "Deleting stale marker comment on #%d", number)
	_, err = s.client.Issues.DeleteComment(ctx, s.owner, s.repo, existing.GetID())
	_, err = s.skipMarkerCommentIfForbidden(ctx, number, "deleting marker comment", err)
	return err
}

// findMarkerComment returns the first comment on the given issue/PR number
// whose body begins with marker, paging through all comments. Returns nil when
// none match. The prefix match (rather than a substring search) avoids matching
// a human reply that merely quotes the marker comment. The ListComments error is
// returned unwrapped so callers can classify it (see skipMarkerCommentIfForbidden).
func (s *Session[T]) findMarkerComment(ctx context.Context, number int, marker string) (*github.IssueComment, error) {
	opts := &github.IssueListCommentsOptions{PerPage: 100}
	for {
		comments, resp, err := s.client.Issues.ListComments(ctx, s.owner, s.repo, number, opts)
		if err != nil {
			return nil, err
		}
		for _, c := range comments {
			if strings.HasPrefix(c.GetBody(), marker) {
				return c, nil
			}
		}
		if resp.NextPage == 0 {
			return nil, nil
		}
		opts.Page = resp.NextPage
	}
}

// skipMarkerCommentIfForbidden classifies an error from a marker-comment API
// call. Marker comments are best-effort, so a missing permission (GitHub 403,
// e.g. the installation was not granted issues:write on this repo) degrades to
// a logged no-op rather than failing the reconcile. It returns (handled, err):
// handled is false only when err is nil and the caller should continue (used by
// the list path). On any non-nil error handled is true and err is nil (a
// tolerated 403) or wrapped, so a terminal caller can discard handled and just
// return err.
func (s *Session[T]) skipMarkerCommentIfForbidden(ctx context.Context, number int, op string, err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if ge, ok := errors.AsType[*github.ErrorResponse](err); ok && ge.Response != nil && ge.Response.StatusCode == http.StatusForbidden {
		clog.WarnContext(ctx, "Skipping marker comment: insufficient permission", "number", number, "op", op)
		return true, nil
	}
	return true, fmt.Errorf("%s: %w", op, err)
}

// StoredData returns the PRData embedded in the existing PR body, or false if
// no PR exists or the data cannot be extracted. Callers can use this on
// ITERATION passes to recover request metadata that was embedded during the
// FRESH pass, avoiding the need for in-process caches that break under
// multi-replica deployments.
func (s *Session[T]) StoredData() (*T, bool) {
	if s.prBody == "" {
		return nil, false
	}
	data, err := s.manager.Extract(s.prBody)
	if err != nil {
		return nil, false
	}
	return data, true
}

// Findings returns the list of findings to be addressed.
// Returns nil if no PR exists or if all checks passed.
func (s *Session[T]) Findings() []callbacks.Finding {
	return s.findings
}

// HasUnresolvedReviews reports whether the PR carries review feedback the bot
// still owes a response to. A review body on the current head commit always
// counts: bodies carry no reply or resolution and drop out once the bot pushes a
// new commit, so a body still present is unaddressed feedback. A review thread
// counts only while it awaits the bot, meaning its most recent relevant comment
// is not the bot's own reply (see threadAwaitsReply); a thread the bot answered
// last is settled until a trusted author replies again, and a resolved thread is
// never a finding. Returns false if no PR exists.
//
// Callers gate ResetCommitBudget on this so review feedback grants a fresh
// commit budget. Because a thread the bot has answered no longer counts, a
// refuted finding the bot replied to and left open cannot renew the budget every
// round.
func (s *Session[T]) HasUnresolvedReviews() bool {
	for _, f := range s.findings {
		if f.Kind != callbacks.FindingKindReview {
			continue
		}
		if strings.HasPrefix(f.Identifier, reviewBodyIdentifierPrefix) {
			return true
		}
		if _, awaiting := s.reviewThreadsAwaitingReply[f.Identifier]; awaiting {
			return true
		}
	}
	return false
}

// findingByID returns the session finding matching kind and identifier, or an
// error naming the identifier when the session carries no such finding. It is
// the shared lookup behind every finding callback: a callback acts only on a
// finding this session discovered, so a caller-supplied identifier (which may
// originate from model output derived from untrusted review text) that matches
// no finding is refused before any GitHub read or write.
func (s *Session[T]) findingByID(kind callbacks.FindingKind, identifier string) (callbacks.Finding, error) {
	for _, f := range s.findings {
		if f.Kind == kind && f.Identifier == identifier {
			return f, nil
		}
	}
	return callbacks.Finding{}, fmt.Errorf("finding not found: %s/%s", kind, identifier)
}

// FindingCallbacks returns callbacks for fetching finding details.
// The returned callbacks can be embedded into agent tool callbacks.
// Since all details are pre-fetched in NewSession, this just does a lookup.
//
// Every callback validates its identifier against the session's own findings
// before acting, so a caller cannot direct a GitHub read or mutation at a
// finding this session never surfaced.
//
// Reply is set only when the manager enabled it (WithFindingReplies). Left nil,
// the reply tool is not registered, so a consumer whose prompt never mentions
// replies does not carry the capability.
func (s *Session[T]) FindingCallbacks() callbacks.FindingCallbacks {
	cb := callbacks.FindingCallbacks{
		Findings: s.findings,
		GetDetails: func(_ context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			f, err := s.findingByID(kind, identifier)
			if err != nil {
				return "", err
			}
			return f.Details, nil
		},
		GetLogs: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			f, err := s.findingByID(kind, identifier)
			if err != nil {
				return "", err
			}
			return fetchFindingLogs(ctx, s.client, s.owner, s.repo, f)
		},
		Retry: func(ctx context.Context, kind callbacks.FindingKind, identifier string) error {
			if kind != callbacks.FindingKindCICheck {
				return fmt.Errorf("retry is only supported for CI check findings, got: %s", kind)
			}
			f, err := s.findingByID(kind, identifier)
			if err != nil {
				return err
			}
			return rerunCICheck(ctx, s.client, s.owner, s.repo, f)
		},
		Resolve: func(ctx context.Context, identifier string) error {
			if strings.HasPrefix(identifier, reviewBodyIdentifierPrefix) {
				return errors.New("cannot resolve review body findings, only review thread findings can be resolved")
			}
			if _, err := s.findingByID(callbacks.FindingKindReview, identifier); err != nil {
				return err
			}
			return resolveReviewThread(ctx, s.gqlClient, identifier)
		},
	}

	if s.manager != nil && s.manager.findingReplies {
		cb.Reply = func(ctx context.Context, identifier, body string) error {
			if strings.HasPrefix(identifier, reviewBodyIdentifierPrefix) {
				return errors.New("cannot reply to review body findings, only review thread findings can be replied to")
			}
			if _, err := s.findingByID(callbacks.FindingKindReview, identifier); err != nil {
				return err
			}
			return replyToReviewThread(ctx, s.gqlClient, identifier, sanitizeReplyBody(body))
		}
	}

	return cb
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

// replyToReviewThread posts a reply in a review thread via the GitHub
// addPullRequestReviewThreadReply GraphQL mutation. The thread's node ID is the
// finding identifier already carried from NewSession, so no comment lookup is
// needed — this uses less new client surface than the REST reply-in-thread call,
// which would need the root comment's databaseId, the PR number, and owner/repo.
func replyToReviewThread(ctx context.Context, gqlClient *graphqlclient.GraphQLClient, threadID, body string) error {
	if threadID == "" {
		return errors.New("empty review thread id")
	}
	if body == "" {
		return errors.New("empty reply body")
	}

	var mutation struct {
		AddPullRequestReviewThreadReply struct {
			Comment struct {
				Id string
			}
		} `graphql:"addPullRequestReviewThreadReply(input: $input)"`
	}

	return gqlClient.Mutate(ctx, "AddPullRequestReviewThreadReply", &mutation, githubv4.AddPullRequestReviewThreadReplyInput{
		PullRequestReviewThreadID: githubv4.ID(threadID),
		Body:                      githubv4.String(body),
	}, nil)
}

// ErrNoChanges can be returned by the makeChanges callback to signal that no
// diff was produced. Upsert passes this error through (wrapped) so the caller
// can decide how to handle it (e.g. close an existing PR, log, or ignore).
//
// Upsert also translates clonemanager.ErrNothingToCommit into ErrNoChanges so
// callbacks using clonemanager.MakeAndPushChanges don't need to map it manually.
var ErrNoChanges = errors.New("no changes")

// Upsert creates a new PR or updates an existing one with the provided properties.
// Labels exceeding GitHub's limit are shortened to a 40-character prefix and
// 10-character hash. It only calls makeChanges when refresh is needed: no existing
// PR, merge conflict, CI failures (only when WithFindingsIteration is enabled), or
// embedded data differs.
//
// If makeChanges returns ErrNoChanges (or clonemanager.ErrNothingToCommit, which
// is translated to ErrNoChanges), it is passed through (wrapped) so the caller
// can check for it with errors.Is.
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
	labels = normalizeLabels(labels)

	// Check if refresh is needed
	needsRefresh, err := s.needsRefresh(ctx, data, labels)
	if err != nil {
		return "", err
	}

	if !needsRefresh {
		clog.InfoContext(ctx, "PR is up to date, no refresh needed")
		return s.prURL, nil
	}

	// Make code changes on the branch. clonemanager.ErrNothingToCommit is
	// surfaced when MakeAndPushChanges runs an updateFn that produces no diff;
	// translate it into the standard ErrNoChanges so callers only have to
	// check for one sentinel.
	if err := makeChanges(ctx, s.branchName); errors.Is(err, ErrNoChanges) || errors.Is(err, clonemanager.ErrNothingToCommit) {
		return "", fmt.Errorf("upsert %s: %w", s.branchName, ErrNoChanges)
	} else if err != nil {
		return "", fmt.Errorf("making changes: %w", err)
	}

	// Catch agent-revert: makeChanges pushed commits, but they net-zero against
	// base. When closeOnEmptyDiff is false, fall through to update so the body
	// re-embeds current data and breaks the trigger/agent cycle.
	if s.manager.closeOnEmptyDiff {
		comp, _, err := s.client.Repositories.CompareCommits(ctx, s.owner, s.repo, s.ref, s.branchName, &github.ListOptions{PerPage: 1})
		if err != nil {
			return "", fmt.Errorf("comparing branch to base: %w", err)
		}
		if len(comp.Files) == 0 {
			clog.InfoContextf(ctx, "Branch %s has no aggregate diff against %s", s.branchName, s.ref)
			return "", s.CloseAnyOutstanding(ctx, "Closing PR because all changes have been resolved.")
		}
	}

	// Generate PR title and body from templates
	title, err := s.manager.render(s.manager.titleTemplate, data)
	if err != nil {
		return "", fmt.Errorf("executing title template: %w", err)
	}

	body, err := s.manager.render(s.manager.bodyTemplate, data)
	if err != nil {
		return "", fmt.Errorf("executing body template: %w", err)
	}

	suffix := fmt.Sprintf("\n\n> **Note:** If you need to make manual changes to this PR, apply the `skip:%s` label. This gives full control of the PR to human operators: the automation will not post updates, close the PR, or delete the branch.", s.manager.identity)

	// Append trace ID so developers can map this PR back to the agent trace.
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		suffix += s.manager.traceFooter(ctx, spanCtx.TraceID().String())
	}

	// Persist the caller's data and changemanager metadata in one block; carrying
	// the metadata keeps the commit-budget baseline and the reasoning log across
	// body regenerations.
	tail, err := s.manager.templateExecutor.Embed("", &embeddedData[T]{Data: *data, Meta: s.meta})
	if err != nil {
		return "", fmt.Errorf("embedding data: %w", err)
	}
	body = fitPRBody(ctx, body, suffix, tail)

	if s.prNumber == 0 {
		// Create new PR
		clog.InfoContextf(ctx, "Creating new PR with head %s and base %s", s.branchName, s.ref)

		pr, _, err := s.client.PullRequests.Create(ctx, s.owner, s.repo, &github.NewPullRequest{
			Title: new(title),
			Body:  new(body),
			Head:  new(s.branchName),
			Base:  new(s.ref),
			Draft: new(draft),
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
		Title: new(title),
		Body:  new(body),
		Draft: new(draft),
	})
	if err != nil {
		return "", fmt.Errorf("updating pull request: %w", err)
	}

	// Only add labels missing from the PR, preserving labels set by other bots
	// or humans that this reconciler does not manage.
	existingLabelSet := make(map[string]struct{}, len(freshPR.Labels))
	for _, l := range freshPR.Labels {
		existingLabelSet[l.GetName()] = struct{}{}
	}
	desiredLabelSet := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		desiredLabelSet[l] = struct{}{}
	}
	var missingLabels []string
	for _, l := range labels {
		if _, ok := existingLabelSet[l]; !ok {
			missingLabels = append(missingLabels, l)
		}
	}
	if len(missingLabels) > 0 {
		if _, _, err := s.client.Issues.AddLabelsToIssue(ctx, s.owner, s.repo, s.prNumber, missingLabels); err != nil {
			return "", fmt.Errorf("adding labels: %w", err)
		}
	}

	// Remove managed labels that the reconciler previously set but no longer
	// wants. Labels not declared as managed (set by humans or other bots) are
	// left untouched.
	for _, l := range s.manager.managedLabels {
		if _, desired := desiredLabelSet[l]; desired {
			continue
		}
		if _, present := existingLabelSet[l]; !present {
			continue
		}
		if _, err := s.client.Issues.RemoveLabelForIssue(ctx, s.owner, s.repo, s.prNumber, l); err != nil {
			return "", fmt.Errorf("removing label %q: %w", l, err)
		}
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

	// Compare only the caller's data; metadata changes (budget baseline,
	// reasoning log) must not force a refresh.
	existingJSON, err := json.Marshal(existing.Data)
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

	// Check if the PR carries a managed label that is no longer desired, which
	// Upsert needs to remove (e.g. a "manual review needed" label that should
	// be cleared once the diff that triggered it is gone).
	desiredSet := make(map[string]struct{}, len(desiredLabels))
	for _, l := range desiredLabels {
		desiredSet[l] = struct{}{}
	}
	for _, l := range s.manager.managedLabels {
		if _, onPR := existingSet[l]; !onPR {
			continue
		}
		if _, desired := desiredSet[l]; !desired {
			clog.InfoContextf(ctx, "PR has managed label %q that is no longer desired, refresh needed: existing=%v desired=%v", l, s.prLabels, desiredLabels)
			return true, nil
		}
	}

	return false, nil
}
