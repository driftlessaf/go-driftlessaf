/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	"chainguard.dev/driftlessaf/reconcilers/statemachine"
	"chainguard.dev/driftlessaf/workqueue"
	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-github/v88/github"
	"go.opentelemetry.io/otel/trace"
)

// errIssueInactive cancels publication without treating discarded agent work as
// a no-change result that would post a give-up explanation.
var errIssueInactive = errors.New("issue is no longer eligible for publication")

// reconcileIssue processes an issue URL and runs the agent to create/update a PR.
func (r *Reconciler[Req, Resp, CB]) reconcileIssue(ctx context.Context, res *githubreconciler.Resource, gh *github.Client) error {
	return r.reconcileIssueOnce(ctx, res, gh, false)
}

// reconcileIssueOnce is one pass of reconcileIssue. rechecked reports that an
// earlier pass found the PR's mergeability unknown and waited for GitHub to
// compute it, so this pass does not wait again.
func (r *Reconciler[Req, Resp, CB]) reconcileIssueOnce(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, rechecked bool) error {
	log := clog.FromContext(ctx)

	// Fetch the issue
	issue, _, err := gh.Issues.Get(ctx, res.Owner, res.Repo, res.Number)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}

	// An issue from an untrusted author never runs the agent. The cleanup cases
	// below still apply to it, so a PR it opened earlier closes with it.
	author := issue.GetUser()
	untrusted := r.trustIssueAuthors &&
		!changemanager.TrustedAuthor(issue.GetAuthorAssociation(), author.GetLogin(), author.GetType(), r.trustedIssueAuthors)

	// Create a change session for the PR (needed for skip label check and PR cleanup)
	changeSession, err := r.changeManager.NewSession(ctx, gh, res)
	if err != nil {
		return fmt.Errorf("create change session: %w", err)
	}

	// The issue creator is a bot-managed assignee: assigning the PR to them
	// should not cause ShouldSkip to return true.
	creator := issue.GetUser().GetLogin()

	state := changeSession.State()
	if rechecked {
		log.With("settled", !state.IsUnknown(), "after", r.mergeabilityRecheck.String()).Info("Read PR mergeability again")
	}

	// A human has taken over: honor the skip contract and leave the PR and the
	// issue completely untouched, labels included.
	if changeSession.ShouldSkip(creator) {
		if changeSession.HasSkipLabel() {
			clog.InfoContext(ctx, "PR has skip label, not updating to preserve manual changes", "pr", changeSession.PRNumber())
		} else {
			clog.InfoContext(ctx, "PR is assigned to humans, not updating to avoid stomping their work", "pr", changeSession.PRNumber(), "assignees", changeSession.Assignees())
		}
		return nil
	}
	if changeSession.IssueHasSkipLabel(issue) {
		clog.InfoContext(ctx, "Issue has skip label, leaving it and any PR alone")
		return nil
	}

	// Keep the issue's draft label in sync with the PR. The draft flag only takes
	// effect at PR creation, so once a human promotes the PR out of draft, the
	// label no longer reflects reality — drop it so it stops implying the PR is a
	// draft. This is one-directional (PR state -> label): the bot never re-drafts
	// a PR, so it never re-adds the label. Runs after the skip checks above (a
	// takeover means hands off the issue too) but before the switch so it still
	// covers the green-PR path, which returns early.
	if r.shouldClearDraftLabel(issue, state.HasPR(), changeSession.IsDraft()) {
		if _, err := gh.Issues.RemoveLabelForIssue(ctx, res.Owner, res.Repo, res.Number, r.draftLabel); err != nil {
			log.With("error", err).Warn("Failed to remove draft label from issue after PR was promoted out of draft")
		}
	}

	// Only the unknown-mergeability case below waits on mergeability, so wait
	// only when no earlier case would decide the reconcile. The next pass reads
	// the issue again too, since it can change while this one waits.
	if !rechecked && !untrusted && r.mergeabilityRecheck > 0 && state.IsUnknown() && issue.GetState() != "closed" &&
		(r.requiredLabel == "" || hasLabel(issue, r.requiredLabel)) &&
		!state.HitMaxCommits() && !state.HasFindings() && !state.HasPendingChecks() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.mergeabilityRecheck):
		}
		return r.reconcileIssueOnce(ctx, res, gh, true)
	}

	// A steering note the PR has not recorded yet. The run that consumes it
	// records it again in the PR's data, so a note is acted on once.
	var steeringID string
	var steeringNew bool
	if r.steering {
		steeringID = newestSteeringID(issue.GetBody())
		if steeringID != "" && state.HasPR() {
			var recorded string
			if stored, err := changeSession.Extract(); err == nil && stored != nil {
				recorded = stored.SteeringID
			}
			steeringNew = steeringID != recorded
		}
	}

	// The evidence the PR already records. It rides along in the data this run
	// compares against the PR, so that a PR whose direction is unchanged is
	// not refreshed just because its evidence differs from a run's. A run
	// that executes the agent replaces it.
	//
	// storedRaw is the value as stored, for the refresh comparison, so a
	// record that fails validation does not read as a change on every run.
	var storedRaw *Evidence
	if state.HasPR() {
		if stored, err := changeSession.Extract(); err == nil && stored != nil {
			storedRaw = stored.Evidence
		}
	}

	var usePRBranch bool
	// revision stamps a give-up note posted by this reconcile. It is read
	// before the agent runs, so a reply that arrives during the run is not
	// counted as answered and gets a run of its own.
	var revision string
	switch {
	case r.requiredLabel != "" && !hasLabel(issue, r.requiredLabel):
		clog.InfoContext(ctx, "Issue missing required label, closing any outstanding PRs", "required_label", r.requiredLabel)
		r.giveUp.Clear(ctx, changeSession)
		// A human took the issue back. Drop any issue-side give-up comment too,
		// so re-labeling the issue (the usual way to ask for another attempt)
		// starts from a clean slate rather than a stale explanation.
		r.clearIssueGiveUp(ctx, issue, changeSession)
		prURL := changeSession.PRURL()
		if err := changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue no longer has the required label."); err != nil {
			return err
		}
		// Only a PR actually closed just now is a transition; a labelless
		// issue with no PR re-reconciling is not.
		if state.HasPR() {
			r.emitTransition(ctx, issue, prURL,
				statemachine.StatusFailed, statemachine.FailureModePRClosed, TriggerRequiredLabelRemoved)
		}
		return nil

	case issue.GetState() == "closed":
		clog.InfoContext(ctx, "Issue is closed, closing any outstanding PRs")
		r.giveUp.Clear(ctx, changeSession)
		prURL := changeSession.PRURL()
		if err := changeSession.CloseAnyOutstanding(ctx, "Closing PR because the issue was closed."); err != nil {
			return err
		}
		// With no persisted prior state this re-emits if a closed issue
		// reconciles again (rare: closed issues stop generating events);
		// latest-transition consumers are unaffected since the terminal
		// status is unchanged.
		to, mode := closedIssueTransition(state.HasPR(), issue.GetStateReason())
		if !state.HasPR() {
			prURL = ""
		}
		r.emitTransition(ctx, issue, prURL, to, mode, TriggerIssueClosed)
		return nil

	case untrusted:
		log.With("author", author.GetLogin(), "association", issue.GetAuthorAssociation()).Info("Issue author is not trusted, skipping")
		return nil

	case r.updateBranchLabel != "" && state.HasPR() && changeSession.HasLabel(r.updateBranchLabel):
		return r.updateBranch(ctx, res, gh, changeSession)

	case state.NeedsRebase():
		clog.InfoContext(ctx, "PR needs rebase, starting fresh from default branch")

	case state.HitMaxCommits():
		// Unresolved review feedback grants a fresh commit budget (a no-op
		// unless WithDynamicCommitBudget is enabled). If the limit still holds
		// after the reset, the turn limit stands; otherwise iterate on the
		// existing PR to address the review.
		if changeSession.HasUnresolvedReviews() {
			changeSession.ResetCommitBudget(ctx)
		}
		if changeSession.State().HitMaxCommits() {
			clog.InfoContext(ctx, "PR hit turn limit")
			// The label edge is the transition: ApplyTurnLimit is idempotent, so
			// only the reconcile that newly applies the label emits.
			newlyLimited := !changeSession.HasTurnLimitLabel()
			prURL, err := changeSession.ApplyTurnLimit(ctx)
			if err == nil && newlyLimited {
				r.emitTransition(ctx, issue, prURL,
					statemachine.StatusFailed, statemachine.FailureModeMaxTurns, statemachine.TriggerMaxTurns)
			}
			return err
		}
		clog.InfoContext(ctx, "PR hit turn limit but has unresolved reviews, iterating with fresh commit budget")
		usePRBranch = true

	case state.HasFindings():
		log.With("findings", len(changeSession.Findings())).Info("PR has CI findings, iterating")
		usePRBranch = true

	case state.HasPendingChecks():
		log.With("pending_checks", changeSession.PendingChecks()).Info("PR has pending checks, skipping")
		return nil

	// Mergeability not yet computed, with nothing else to act on (findings and
	// pending checks handled above). Requeue to re-check rather than falling
	// through, which resets the PR from the default branch and re-runs the agent
	// for nothing. A reconciler that disables the requeue gets that reset.
	case state.IsUnknown() && r.unknownMergeabilityRequeueAfter > 0:
		log.With("after", r.unknownMergeabilityRequeueAfter).Info("PR mergeability still being computed by GitHub, requeuing")
		return workqueue.RequeueAfter(r.unknownMergeabilityRequeueAfter)

	case steeringNew:
		log.With("steering_id", steeringID).Info("Issue has a new steering note, iterating on the PR")
		usePRBranch = true

	case state.HasNoConflicts():
		log.Info("PR is green, leaving it for human review")
		newlyReady := !changeSession.HasReadyForReviewLabel()
		prURL, err := changeSession.ApplyReadyForReview(ctx)
		if err != nil {
			return fmt.Errorf("apply ready-for-review: %w", err)
		}
		if newlyReady {
			// Still active — the PR now waits on humans, and time since this
			// transition measures how long it has waited.
			r.emitTransition(ctx, issue, prURL,
				statemachine.StatusActive, "", TriggerReadyForReview)
		}
		// The PR is green: drop any give-up comment from a prior iteration, since
		// the PR recovered without the agent needing to push a fix. Clear is a
		// no-op when no comment exists, so this is safe to run on every green pass.
		// The issue side gets the same treatment: the clear that runs when the PR
		// opens swallows its errors, so this is the retry that keeps a stale
		// "nothing to materialize" from outliving the PR that disproved it.
		r.giveUp.Clear(ctx, changeSession)
		r.clearIssueGiveUp(ctx, issue, changeSession)
		return nil

	case !state.HasPR():
		var answered bool
		revision, answered = r.giveUpRevision(ctx, changeSession, issue)
		if answered {
			log.Info("This bot's give-up note already answers the issue as it stands, not running again")
			return nil
		}
		log.Info("No existing PR, creating from scratch")

	default:
		log.With("state", state).Warn("Unexpected state combination")
	}

	// Announce work has started on the issue before the first PR appears. Only
	// when no PR exists yet, so new commits on an open PR never retrigger it; the
	// marker dedup keeps it to one comment across repeated no-PR reconciles (a
	// first attempt may end in ErrNoChanges or a transient agent error).
	//
	// TestReconcileIssueGuardWithStartComment covers this gate through
	// reconcileIssue with no PR; no test reconciles with a PR open, so the
	// HasPR half is untested. TestStartCommentSurface covers surface() in
	// isolation.
	if !state.HasPR() {
		r.startComment.surface(ctx, changeSession)
	}

	// Build the request before Upsert so it can be stored in PRData.
	request, err := r.buildRequest(ctx, issue, changeSession)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	// Captured from the agent execution inside the Upsert closure. Used after
	// Upsert to derive PR labels (WithPRLabelsFromResult) and to surface a
	// give-up comment (WithGiveUpComment). agentRan gates both uses: Upsert skips
	// the closure when the PR is already up to date, leaving result as the zero
	// value, so neither must see it then.
	var result Resp
	var agentRan bool

	// The commit the lease checked out for the agent, "" until it is leased.
	var checkoutHead string

	// The commit a push left at the branch tip and the evidence the run
	// records for it, for the evidence check. Empty when nothing was pushed.
	var pushedHead string
	var pushedEvidence *Evidence

	// Carry the issue's labels onto the PR when configured, so labels added to
	// the issue (e.g. to enable optional review) propagate situationally rather
	// than always. Merged with the fixed prLabels; deduped so a label present in
	// both is stamped once.
	prLabels := r.prLabelsForIssue(issue)

	// Open the PR as a draft when the issue opts in via the configured draft
	// label. Only meaningful at creation; GitHub ignores draft on REST edits.
	draft := r.draftForIssue(issue)

	// Create/update the PR with the changes. prData is passed by pointer and
	// the title and body templates render only after the closure below runs, so
	// the fields that the closure sets after the agent executes
	// (ReasoningSummary, Headline, VariantSummary) are visible to the templates.
	prData := &PRData[Req]{
		Identity:      r.identity,
		IssueURL:      issue.GetHTMLURL(),
		IssueNumber:   issue.GetNumber(),
		IssueBodyHash: sha256.Sum256([]byte(issue.GetBody())),
		SteeringID:    steeringID,
		Evidence:      storedRaw,
		Request:       request,
	}
	// The run consumes the note, so the PR is no longer ready for review as it
	// stands. The green path restores the label once the PR settles.
	if steeringNew {
		if _, err := changeSession.ClearReadyForReview(ctx); err != nil {
			return fmt.Errorf("clear ready-for-review: %w", err)
		}
	}
	prURL, err := changeSession.Upsert(ctx, prData, draft, prLabels, func(ctx context.Context, branchName string) error {
		// Tee the agent's completed trace so the PR body template can render
		// a rationale summary via {{.ReasoningSummary}} (see
		// ReasoningSummarySnippet): per-action tool-call reasoning when
		// present, falling back to extended-thinking blocks. No-op when the
		// run produced neither.
		ctx, captured := agenttrace.CaptureTrace[Resp](ctx)
		// The evidence check this bot wrote for the head, intent and
		// findings already records an attempt, and its own check event
		// queued this run. Running the agent again would loop on that
		// event for as long as the outcome kept changing.
		recorded, err := r.evidenceAttemptRecorded(ctx, gh, res, prData, changeSession.HeadSHA(), changeSession.Findings())
		if err != nil && state.HasPR() {
			return err
		}
		if recorded && state.HasPR() && usePRBranch {
			log.Info("Evidence check already records an attempt for this head, intent and findings, not running the agent")
			return changemanager.ErrNoChanges
		}
		cloneMgr, err := r.cloneMeta.Get(res.Owner, res.Repo)
		if err != nil {
			return fmt.Errorf("get clone manager: %w", err)
		}

		// Lease based on current state:
		// - CI failures on a mergeable PR: lease PR branch for iteration
		// - Otherwise (no PR, needs rebase, or fresh run): lease default branch
		var lease *clonemanager.Lease
		if usePRBranch {
			log.With("branch", branchName).Info("Acquiring clone lease for pull request branch")
			lease, err = cloneMgr.LeaseRef(ctx, res, branchName,
				clonemanager.WithCommitDepth(changeSession.CommitCount()+1))
		} else {
			log.Info("Acquiring clone lease for default branch")
			lease, err = cloneMgr.Lease(ctx, res)
		}
		if err != nil {
			return fmt.Errorf("acquire lease: %w", err)
		}
		defer func() {
			if err := lease.Return(ctx); err != nil {
				log.With("error", err).Warn("Failed to return lease")
			}
		}()
		// The commit the agent actually sees, before it edits anything.
		if head, herr := lease.Repo().Head(); herr == nil {
			checkoutHead = head.Hash().String()
		}

		// Run the agent and push changes. The commit scope the lease installs on
		// the update context is captured here, because the commit runs after the
		// update function returns and the scope is where the guard records what
		// it left out (see commitscope.Scope.Dropped).
		var scope *commitscope.Scope
		err = lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *gogit.Worktree) (string, error) {
			scope, _ = commitscope.ScopeFromContext(ctx)

			cbs, err := r.buildCallbacks(ctx, changeSession, lease)
			if err != nil {
				return "", fmt.Errorf("build callbacks: %w", err)
			}

			// A request that carries the checked-out worktree root has it set
			// here, where the worktree exists, so an agent-side post-execute step
			// can operate on the checkout. Requests that do not implement the
			// setter are unaffected.
			if s, ok := any(request).(worktreeRootSetter); ok {
				s.SetWorktreeRoot(wt.Filesystem.Root())
			}

			result, err = r.agent.Execute(ctx, request, cbs)
			if err != nil {
				return "", fmt.Errorf("execute agent: %w", err)
			}
			agentRan = true
			prData.ReasoningSummary = agenttrace.SummarizeTraceReasoning(captured(), reasoningSummaryMaxChars)
			if r.prRenderFromResult != nil {
				render := r.prRenderFromResult(result)
				prData.Headline = prHeadline(changeSession.Headline(), render.Headline, usePRBranch)
				prData.VariantSummary = render.VariantSummary
			}

			// Check if the agent left the worktree clean (no file changes).
			// Return ErrNoChanges so Upsert can propagate it to the caller.
			status, err := wt.Status()
			if err != nil {
				return "", fmt.Errorf("get worktree status: %w", err)
			}
			if status.IsClean() {
				return "", changemanager.ErrNoChanges
			}

			// The issue may have closed or been taken over while the agent ran.
			// Refresh only when there is a change to commit, before the lease
			// pushes it and Upsert creates or updates the PR.
			freshIssue, _, err := gh.Issues.Get(ctx, res.Owner, res.Repo, res.Number)
			if err != nil {
				return "", fmt.Errorf("refresh issue before publication: %w", err)
			}
			if freshIssue.GetState() != "open" ||
				(r.requiredLabel != "" && !hasLabel(freshIssue, r.requiredLabel)) ||
				changeSession.IssueHasSkipLabel(freshIssue) {
				clog.InfoContext(ctx, "Issue no longer permits publication, discarding agent changes", "issue_state", freshIssue.GetState())
				return "", errIssueInactive
			}

			// The workflow-edit declaration is read from the same fresh copy, at
			// the moment the rule applies: a label removed while the agent ran
			// does not lift it, and one added meanwhile does.
			r.declareWorkflowEdit(freshIssue, scope)

			// A commit is certain, so the regenerated body persists the
			// headline for the runs that stack fix-up commits on this one.
			changeSession.SetHeadline(prData.Headline)
			return result.GetCommitMessage(), nil
		})
		// A pushed change is evaluated at the commit now at the branch tip. The
		// evidence names it, and the checks seen on the input head stay bound
		// to that head. Without a known tip or a declared verdict the PR
		// records none, so an earlier attempt's evidence never stands for
		// this commit.
		if err == nil && agentRan {
			prData.Evidence = nil
			if head, herr := lease.Repo().Head(); herr == nil {
				// The input head is the commit the lease actually checked
				// out. The checks were read for the session's head, so they
				// are kept only when the checkout is that same commit.
				inputHead, inputFindings := checkoutHead, changeSession.Findings()
				if !usePRBranch || checkoutHead == "" || checkoutHead != changeSession.HeadSHA() {
					// The agent ran on the default branch checkout, or on a
					// commit other than the one the checks were read for.
					inputFindings = nil
				}
				if !usePRBranch {
					inputHead = ""
				}
				prData.Evidence = newEvidence(prData, result, head.Hash().String(), inputHead, inputFindings)
				// A contradictory declaration cannot attest the pushed commit.
				if prData.Evidence != nil && prData.Evidence.Verdict != EvidenceVerdictChanged {
					prData.Evidence = nil
				}
				pushedHead, pushedEvidence = head.Hash().String(), prData.Evidence
			}
		}
		// Whatever the commit's outcome, the guard's drops are known now: the
		// body template renders them on a PR that carries the rest of the
		// change, and the result can name them in its no-change explanation
		// when nothing was left to commit. A nil scope (guard off) reports
		// none.
		prData.Dropped = scope.Dropped()
		if agentRan {
			if s, ok := any(result).(droppedSetter); ok {
				s.SetDropped(prData.Dropped)
			}
		}
		return err
	})
	if err != nil {
		if errors.Is(err, errIssueInactive) {
			return nil
		}
		if errors.Is(err, changemanager.ErrNoChanges) {
			log.Info("No changes after agent execution, nothing to commit")
			// The agent read the note and left the branch as it was. Record
			// the note so later events do not run it again.
			// The branch is as it was, so the head the agent evaluated is the
			// PR head the run started from, but only when the agent ran on the
			// PR branch. On the default branch it never saw that head, so no
			// evidence is recorded (unknown).
			// With the evidence check on, the attempt's result lives there and
			// the PR data keeps the record it had.
			var noOpRecord *Evidence
			if agentRan {
				prData.Evidence = nil
				// The head is the commit actually checked out under the lease,
				// and it must be the head this session saw on the PR. A
				// default-branch run, or a checkout that is not the session's
				// head, makes no claim about that head.
				if state.HasPR() && usePRBranch && checkoutHead != "" && checkoutHead == changeSession.HeadSHA() {
					prData.Evidence = newEvidence(prData, result, checkoutHead, checkoutHead, changeSession.Findings())
					// Nothing landed, so a "changed" declaration (a commit
					// message the guard emptied, or a clean worktree) fails
					// validation on the unchanged head and leaves no record:
					// the evidence stays unknown rather than claiming an edit.
				}
				if r.evidenceCheck != nil {
					noOpRecord, prData.Evidence = prData.Evidence, storedRaw
				}
			}
			// Preserve the existing write for an explicit steering note. A new
			// attempt or issue edit alone never owns the PR description.
			if agentRan && steeringNew {
				// Evidence claims a head, so the write confirms the live PR
				// still has that head and the body this run read (no newer
				// push, steer or takeover). A consumer must verify the live
				// head, the current intent and the checks directly: editable
				// PR text cannot authenticate GitHub checks.
				var err error
				if prData.Evidence != nil && r.evidenceCheck == nil {
					err = changeSession.ReembedDataAtHead(ctx, prData, prData.Evidence.HeadSHA)
				} else {
					// No head is claimed: keep the long-standing
					// data-block-only write, which preserves a person's
					// prose edits to the description.
					err = changeSession.ReembedData(ctx, prData)
				}
				if err != nil {
					return fmt.Errorf("record steering ID and evidence: %w", err)
				}
			}
			// The latest attempt's result for the head the run saw replaces
			// any earlier one on that commit, a "changed" from a push
			// included, and an attempt that cannot stand behind a verdict
			// leaves it unknown. The session's PR head is the commit the
			// result is about even when the run could not claim it.
			if agentRan && state.HasPR() && r.evidenceCheck != nil {
				// The clean-worktree path returns before the fresh-issue
				// check, so a skip label applied while the agent ran is
				// read here and leaves the check alone.
				freshIssue, _, err := gh.Issues.Get(ctx, res.Owner, res.Repo, res.Number)
				if err != nil {
					return fmt.Errorf("refresh issue before evidence check: %w", err)
				}
				freshPR, _, err := gh.Issues.Get(ctx, res.Owner, res.Repo, changeSession.PRNumber())
				if err != nil {
					return fmt.Errorf("refresh PR before evidence check: %w", err)
				}
				if changeSession.IssueHasSkipLabel(freshIssue) || changeSession.IssueTakenOver(freshPR, creator) {
					clog.InfoContext(ctx, "Skip label or human assignee appeared while the agent ran, not publishing the evidence check")
				} else {
					// Publish on the commit the agent actually saw. When the
					// PR advanced between the session read and the checkout,
					// that is the new tip, and the record is unknown there.
					evalHead := changeSession.HeadSHA()
					checkoutIsSession := true
					if usePRBranch && checkoutHead != "" {
						evalHead = checkoutHead
						checkoutIsSession = checkoutHead == changeSession.HeadSHA()
					}
					details := newEvidenceCheckDetails(prData, evalHead, noOpRecord)
					// The digest only suppresses PR-branch findings
					// iterations, so a default-branch run never sets it.
					if checkoutIsSession && usePRBranch {
						details.FindingsDigest = findingsDigest(changeSession.Findings())
					}
					if err := r.publishEvidenceCheck(ctx, gh, res, details); err != nil {
						return err
					}
				}
			}
			switch {
			case !agentRan:
				// Upsert skipped the closure (PR already up to date): result
				// is the zero value, so there is nothing to surface.
			case state.HasPR():
				// SurfaceResult applies the give-up label when the result
				// carries an explanation; the session's label cache reflects
				// that immediately, so the before/after pair is the "newly
				// gave up" edge to emit on.
				//
				// This edge only fires for bots that wire WithGiveUpComment
				// (today: linear-materializer). Without it SurfaceResult
				// no-ops, the label never appears, and a no-diff run emits
				// nothing — on every retry, not just the first.
				gaveUpBefore := changeSession.HasGaveUpLabel()
				r.giveUp.SurfaceResult(ctx, changeSession, result)
				if !gaveUpBefore && changeSession.HasGaveUpLabel() {
					r.emitTransition(ctx, issue, changeSession.PRURL(),
						statemachine.StatusFailed, statemachine.FailureModeNoDiff, statemachine.TriggerNoDiff)
				}
			default:
				// The agent gave up before any PR existed, so there is no PR
				// to comment on: surface the explanation on the issue, or the
				// filer sees the start comment and then silence. A note
				// posted for a new revision is the edge to emit on, and a
				// note already posted for this one is not. A new revision is
				// a new attempt, so a give-up after a reply, edit, re-label,
				// or reopen emits again even when the status repeats.
				if r.giveUp.SurfaceResultOnIssue(ctx, changeSession, revision, result) {
					r.emitTransition(ctx, issue, "",
						statemachine.StatusFailed, statemachine.FailureModeNoDiff, statemachine.TriggerNoDiff)
				}
			}
			return nil
		}
		r.noteRetriesExhausted(ctx, changeSession, state.HasPR(), revision, err)
		return fmt.Errorf("upsert PR: %w", err)
	}

	// The agent pushed a fix: clear any stale give-up comment from a prior
	// iteration where it had nothing to do — on the PR, and on the issue if
	// an earlier attempt gave up before any PR existed (the PR now supersedes
	// that explanation). Only this run can have opened the first PR, so with a
	// PR already in hand there is no issue-side comment to look for.
	r.giveUp.Clear(ctx, changeSession)
	if !state.HasPR() {
		r.clearIssueGiveUp(ctx, issue, changeSession)
	}

	// The pushed commit's result, so a reader of the evidence check sees the
	// push and not an earlier attempt. A commit that was not pushed has no
	// check of its own.
	if agentRan && prURL != "" && pushedHead != "" && r.evidenceCheck != nil {
		if err := r.publishEvidenceCheck(ctx, gh, res, newEvidenceCheckDetails(prData, pushedHead, pushedEvidence)); err != nil {
			clog.WarnContext(ctx, "Failed to publish the evidence check for the push", "error", err)
		}
	}

	// Every push is a genuine transition (back) to active: the initial run
	// that created the PR, a findings iteration, or a post-conflict
	// regeneration. Upsert skips the agent closure when the PR is already up
	// to date, so agentRan gates re-observations out. state still reflects
	// the pre-Upsert session, so HasPR distinguishes the initial run.
	//
	// An empty prURL with a nil error is the closeOnEmptyDiff path: the agent
	// pushed commits that net to zero against base, so Upsert closed the PR
	// (or never opened one). Emitting active there would pin the issue
	// "active" on the overlay with no PR that could ever move it again —
	// every genuine create/update path returns a non-empty URL.
	if agentRan && prURL != "" {
		r.emitTransition(ctx, issue, prURL,
			statemachine.StatusActive, "", upsertTrigger(!state.HasPR(), usePRBranch, state.NeedsRebase()))
	}

	// Assign the PR to the issue creator so they can easily find it.
	if creator != "" {
		if err := changeSession.AddAssignees(ctx, []string{creator}); err != nil {
			log.With("error", err).Warn("Failed to assign PR to issue creator")
		}
	}

	// Stamp result-derived labels on the PR (opt-in via WithPRLabelsFromResult),
	// pruning stale labels under the replace-managed prefixes (opt-in via
	// WithReplaceLabelPrefixes). Skipped when the agent did not run (PR already
	// up to date): result is the zero value then, so deriving labels from it
	// would be meaningless.
	if r.prLabelsFromResult != nil && agentRan {
		if err := changeSession.ReplaceLabels(ctx, r.prLabelsFromResult(result), r.replaceLabelPrefixes); err != nil {
			clog.WarnContext(ctx, "Failed to stamp result-derived labels on PR", "error", err)
		}
	}

	log.With("pr_url", prURL).Info("PR created/updated")
	return nil
}

// updateBranch handles the update-branch label on the PR: it merges the default
// branch into the PR branch through GitHub when the branch lacks it, then
// removes the label. The agent does not run. The merge push starts CI and a
// normal reconcile.
func (r *Reconciler[Req, Resp, CB]) updateBranch(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, session *changemanager.Session[PRData[Req]]) error {
	prNumber := session.PRNumber()

	// Act only on a branch in this repository: a fork PR's head lives in the
	// fork, so the same-named branch here is not part of that PR.
	pr, _, err := gh.PullRequests.Get(ctx, res.Owner, res.Repo, prNumber)
	if err != nil {
		return fmt.Errorf("get PR: %w", err)
	}
	if !strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), res.Owner+"/"+res.Repo) {
		clog.InfoContext(ctx, "PR head is not in this repository, dropping the label without updating", "pr", prNumber, "head", pr.GetHead().GetRepo().GetFullName())
		return r.removeUpdateBranchLabel(ctx, res, gh, prNumber)
	}

	repo, _, err := gh.Repositories.Get(ctx, res.Owner, res.Repo)
	if err != nil {
		return fmt.Errorf("get repository: %w", err)
	}
	cmp, _, err := gh.Repositories.CompareCommits(ctx, res.Owner, res.Repo, repo.GetDefaultBranch(), session.HeadSHA(), &github.ListOptions{PerPage: 1})
	if err != nil {
		return fmt.Errorf("compare default branch with PR head: %w", err)
	}

	// "ahead" and "identical" mean the PR head contains the default branch.
	if status := cmp.GetStatus(); status != "ahead" && status != "identical" {
		sha := session.HeadSHA()
		_, _, err = gh.PullRequests.UpdateBranch(ctx, res.Owner, res.Repo, prNumber, &github.PullRequestBranchUpdateOptions{ExpectedHeadSHA: &sha})
		_, accepted := errors.AsType[*github.AcceptedError](err)
		ghErr, isGHErr := errors.AsType[*github.ErrorResponse](err)
		switch {
		case err == nil, accepted:
			clog.InfoContext(ctx, "Asked GitHub to update the PR branch", "pr", prNumber)
		case isGHErr && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity:
			clog.InfoContext(ctx, "GitHub refused to update the PR branch, dropping the label", "pr", prNumber, "error", err)
		default:
			return fmt.Errorf("update PR branch: %w", err)
		}
	} else {
		clog.InfoContext(ctx, "PR branch already contains the default branch, dropping the label", "pr", prNumber)
	}

	return r.removeUpdateBranchLabel(ctx, res, gh, prNumber)
}

// removeUpdateBranchLabel removes the update-branch label from the PR. A 404
// counts as removed.
func (r *Reconciler[Req, Resp, CB]) removeUpdateBranchLabel(ctx context.Context, res *githubreconciler.Resource, gh *github.Client, prNumber int) error {
	_, err := gh.Issues.RemoveLabelForIssue(ctx, res.Owner, res.Repo, prNumber, changemanager.NormalizeLabel(r.updateBranchLabel))
	if err == nil {
		return nil
	}
	if ghErr, ok := errors.AsType[*github.ErrorResponse](err); ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("remove update-branch label: %w", err)
}

// prLabelsForIssue returns the labels to stamp on the PR for this issue: the
// fixed prLabels, plus every label on the issue when copyIssueLabels is set.
// Duplicates are collapsed so a label present in both is returned once. When
// copyIssueLabels is off it returns the fixed prLabels unchanged. The draft
// label is never copied from the issue: it is a directive to the bot, already
// honored via the draft flag at PR creation, and nothing removes it from the
// PR after promotion — it would misrepresent a ready PR as draft over there.
func (r *Reconciler[Req, Resp, CB]) prLabelsForIssue(issue *github.Issue) []string {
	if !r.copyIssueLabels {
		return r.prLabels
	}
	seen := make(map[string]struct{}, len(r.prLabels)+len(issue.Labels))
	labels := make([]string, 0, len(r.prLabels)+len(issue.Labels))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		labels = append(labels, name)
	}
	for _, l := range r.prLabels {
		add(l)
	}
	for _, l := range issue.Labels {
		if name := l.GetName(); name != r.draftLabel {
			add(name)
		}
	}
	return labels
}

// declareWorkflowEdit lifts the commit guard's workflow rule for this run when
// the reconciler has a workflow-edit label and the issue carries it. It is
// called with the issue as refreshed just before the commit, the point where
// the rule applies, so the label's state while the agent ran does not matter.
// A nil scope (guard off) is a no-op.
func (r *Reconciler[Req, Resp, CB]) declareWorkflowEdit(issue *github.Issue, scope *commitscope.Scope) {
	if r.workflowEditLabel != "" && hasLabel(issue, r.workflowEditLabel) {
		scope.DeclareWorkflowEdit()
	}
}

// draftForIssue reports whether the generated PR should open as a draft: only
// when a draft label is configured and the issue carries it. Off by default.
func (r *Reconciler[Req, Resp, CB]) draftForIssue(issue *github.Issue) bool {
	return r.draftLabel != "" && hasLabel(issue, r.draftLabel)
}

// shouldClearDraftLabel reports whether the source issue's draft label should be
// removed to stay in sync with the PR: the label is configured and present, a PR
// exists, and that PR is no longer a draft (a human promoted it for review).
// Keeping the label after promotion would misrepresent a ready PR as still draft.
func (r *Reconciler[Req, Resp, CB]) shouldClearDraftLabel(issue *github.Issue, hasPR, prIsDraft bool) bool {
	return r.draftLabel != "" && hasPR && !prIsDraft && hasLabel(issue, r.draftLabel)
}

// giveUpRevision returns the revision a give-up note posted by this reconcile
// carries (see issueRevision), and whether this bot already left a note for
// it, in which case the reconcile must not run the agent. Without that check
// the bot's own note, which comes back as an issue event, and every later
// issue event would run the same work again. It reads nothing for a bot with
// no give-up comment. A failed read is logged and leaves the issue unanswered,
// so the agent runs.
func (r *Reconciler[Req, Resp, CB]) giveUpRevision(ctx context.Context, session *changemanager.Session[PRData[Req]], issue *github.Issue) (string, bool) {
	if r.giveUp == nil || r.giveUp.Marker == "" || r.giveUp.Render == nil {
		return "", false
	}
	thread, err := session.ReadIssueThread(ctx, r.giveUp.Marker)
	if err != nil {
		clog.WarnContext(ctx, "Failed to read the issue thread, running the agent", "error", err)
		thread = &changemanager.IssueThread{}
	}
	revision := issueRevision(issue, thread, r.requiredLabel)
	return revision, thread.HasNote(revision)
}

// retriesExhaustedNoteTimeout bounds posting the retries-exhausted note, which
// runs on a context detached from the attempt's: the attempt may have failed
// because its deadline passed.
const retriesExhaustedNoteTimeout = 30 * time.Second

// noteRetriesExhausted posts a note when err ends the workqueue's attempts on
// the issue: the WithRetriesExhaustedPRNote note on the PR when hasPR, and the
// WithRetriesExhaustedNote note on the issue otherwise. revision is set only
// when this reconcile ran the agent for an issue with no PR and a give-up
// comment is configured, the one case the issue note covers. Failures are
// logged and swallowed.
func (r *Reconciler[Req, Resp, CB]) noteRetriesExhausted(ctx context.Context, session *changemanager.Session[PRData[Req]], hasPR bool, revision string, err error) {
	render := r.retriesExhausted
	if hasPR {
		render = r.retriesExhaustedPR
	}
	if render == nil || r.giveUp == nil || (!hasPR && revision == "") {
		return
	}
	// A rescheduled key is retried, and would find the note answering the
	// issue and stop there.
	if githubreconciler.Reschedules(err) {
		return
	}
	attempt, _ := workqueue.AttemptFromContext(ctx)
	if !attempt.Final() && workqueue.GetNonRetriableDetails(err) == nil {
		return
	}
	var traceID string
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		traceID = sc.TraceID().String()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retriesExhaustedNoteTimeout)
	defer cancel()
	body := render(attempt.Number, traceID)
	if hasPR {
		clog.InfoContext(ctx, "Last attempt failed on an open PR, posting a note on the PR", "attempt", attempt.Number, "pr", session.PRNumber())
		if err := session.UpsertMarkerComment(ctx, r.giveUp.Marker, body); err != nil {
			clog.WarnContext(ctx, "Failed to post retries-exhausted note on PR", "error", err)
		}
		return
	}
	clog.InfoContext(ctx, "Last attempt failed with no PR, posting a note on the issue", "attempt", attempt.Number)
	if _, err := session.UpsertIssueNote(ctx, r.giveUp.Marker, revision, body); err != nil {
		clog.WarnContext(ctx, "Failed to post retries-exhausted note on issue", "error", err)
	}
}

// clearIssueGiveUp drops the give-up notes from the source issue. Finding them
// means reading the issue's comments, and reconcileIssue runs on every issue
// event in the repo, most for issues this bot never touched — so skip the call
// when the issue has no comments at all, since a note to delete is itself
// one.
func (r *Reconciler[Req, Resp, CB]) clearIssueGiveUp(ctx context.Context, issue *github.Issue, session *changemanager.Session[PRData[Req]]) {
	if issue.GetComments() == 0 {
		return
	}
	r.giveUp.ClearIssue(ctx, session)
}

// issueRevision identifies what a give-up note answers, as a hex digest: the
// issue title and body that buildRequest hands the agent, and each signal
// that asks for another attempt at them, namely the latest reply from a
// person, the last time the issue was given requiredLabel, and the last time
// it was reopened. While this bot's note for the current revision is on the
// issue, reconciles do not run the agent; any of those signals changes the
// revision and starts another run. Comments by this bot and by other bots are
// not signals (see changemanager.IssueThread), so the bot's own notes leave the
// revision as it was, and two bots on one issue cannot keep each other running.
func issueRevision(issue *github.Issue, thread *changemanager.IssueThread, requiredLabel string) string {
	h := sha256.New()
	for _, part := range []string{
		issue.GetTitle(),
		issue.GetBody(),
		thread.LatestReplyID,
		timestamp(thread.LabeledAt[requiredLabel]),
		timestamp(thread.ReopenedAt),
	} {
		// Length-prefixed, so no two different sets of parts hash alike.
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// timestamp renders t for issueRevision: "" for the zero time, so a signal
// that has never happened reads the same on every pass.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// prHeadline picks the headline for the PR title. A run that stacks a commit
// on the PR branch keeps the headline recorded by the run that built the
// branch: that commit still carries the change, and the PR title becomes the
// squash commit's subject on the default branch. A run that builds the branch
// from the default branch replaces every earlier commit, so its own headline
// names the change. With no recorded headline, the current one stands.
func prHeadline(recorded, current string, stacksOnPRBranch bool) string {
	if stacksOnPRBranch {
		return cmp.Or(recorded, current)
	}
	return current
}

// hasLabel checks if an issue has a specific label.
func hasLabel(issue *github.Issue, label string) bool {
	for _, l := range issue.Labels {
		if l.GetName() == label {
			return true
		}
	}
	return false
}
