/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/linearreconciler"
	"github.com/chainguard-dev/clog"
)

// SaveCallback fires after a successful state attachment save. The
// linearIssueID is the UUID of the issue this save was for; stateJSON is
// the marshaled bot State (post-Set of framework-managed IssueID/IssueURL).
// Note that stateJSON is the bot's State only — Linear-internal metadata
// keys injected by the underlying StateManager (e.g. comment-tracking IDs)
// are not present.
//
// Returns no error: the Linear attachment is the source of truth and a
// stale downstream consumer must not block reconciliation. Implementations
// are expected to handle their own errors (e.g. log at Warn) and never
// panic. If you need to derive the just-appended StateTransition,
// unmarshal stateJSON and read the tail of History.
type SaveCallback func(ctx context.Context, linearIssueID string, stateJSON []byte)

// StateManager is a generic wrapper around linearreconciler.StateManager
// that adds state-machine bookkeeping. It is constructed by
// Reconciler.NewStateManager(issue), parameterised over the bot's wrapper
// State type.
//
// Usage shape:
//
//	mgr := r.NewStateManager(issue)
//	state, _, err := mgr.Load(ctx)
//	state.SetStatus(metareconciler.StatusActive)
//	state.SetPRURL(prURL)
//	ctx = metareconciler.WithTrigger(ctx, metareconciler.TriggerInitialRun)
//	changed, err := mgr.Save(ctx, state)
//
// Save snapshots Status and FailureMode at Load time, diffs them at Save
// time, and appends a StateTransition to History automatically when
// something changed. Callers therefore never write to History directly.
//
// The (changed bool, err error) return from Save lets callers gate
// side-effects like posting a one-time bot comment on PR open.
type StateManager[T any, PT StateConstraint[T]] struct {
	sm                    *linearreconciler.StateManager
	snapshotStatus        Status
	snapshotFailureMode   FailureMode
	snapshotPRURL         string
	snapshotFindings      []FindingRef
	snapshotPendingChecks []string
	snapshotNoDiffCount   int
	snapshotLinearType    string
	snapshotLinearName    string
	loaded                bool
	now                   func() time.Time // injectable clock for tests
	cb                    SaveCallback
	emitter               *transitionEmitter // nil disables transition CloudEvents

	// Captured from the *Issue at NewStateManager time. Save calls
	// SetIssueID/SetIssueURL on the bot's State pointer before persisting
	// so the Linear attachment is self-contained — downstream consumers
	// have everything needed to render a clickable Linear link without an
	// extra API call. Bots never set these themselves; the StateMachine
	// methods are promoted from the embedded metareconciler.State.
	//
	// issueURL is captured once and not refreshed across reconciles. In
	// practice Linear identifiers are stable for the life of an issue so
	// the URL doesn't change, and a re-reconcile after a hypothetical
	// rename would refresh it via NewStateManager.
	issueID  string
	issueURL string
}

// SetSaveCallback configures a post-save hook on this StateManager. Returns
// the same manager for chainability with NewStateManager. Replaces any
// previously-set callback.
//
// Callers using Reconciler.NewStateManager get the Reconciler's
// WithSaveCallback option threaded in automatically; callers using the free
// NewStateManager function (e.g. RepoTargetResolver implementations
// constructed before a Reconciler exists) chain SetSaveCallback to opt in.
func (m *StateManager[T, PT]) SetSaveCallback(cb SaveCallback) *StateManager[T, PT] {
	m.cb = cb
	return m
}

// NewStateManager returns a fresh state-machine-aware manager for the given
// issue, bound to the Reconciler's linearClient. Each manager is single-use
// within one reconcile pass; do not share across goroutines or reuse across
// reconciles (it caches the snapshot from Load to diff against at Save).
//
// Threads through any SaveCallback supplied via WithSaveCallback so every
// save mirrored downstream sees the same hook.
func (r *Reconciler[Req, Resp, CB, T, PT]) NewStateManager(issue *linearreconciler.Issue) *StateManager[T, PT] {
	mgr := NewStateManager[T, PT](r.linearClient, issue).SetSaveCallback(r.saveCallback)
	mgr.emitter = r.transitionEmitter
	return mgr
}

// NewStateManager is the free-function form of (*Reconciler).NewStateManager,
// for callers that need a state-machine-aware manager before the Reconciler
// is constructed (e.g. a RepoTargetResolver supplied as an option to New).
// Type parameters must be spelled out explicitly:
//
//	mgr := metareconciler.NewStateManager[mybot.State, *mybot.State](client, issue)
//
// Precondition: issue.URL should be populated (e.g. via a GetIssue
// round-trip) before construction. The URL is captured here once and
// injected into the persisted JSON on every Save so downstream consumers
// can render clickable links without an extra Linear lookup. If the
// caller passes an *Issue from e.g. a webhook payload that omits URL,
// the persisted issue_url field is empty (omitempty) until a subsequent
// reconcile re-fetches and reconstructs the manager.
func NewStateManager[T any, PT StateConstraint[T]](client *linearreconciler.Client, issue *linearreconciler.Issue) *StateManager[T, PT] {
	return &StateManager[T, PT]{
		sm:       client.NewStateManager(issue),
		now:      time.Now,
		issueID:  issue.ID,
		issueURL: issue.URL,
	}
}

// Load deserializes the state attachment into a fresh PT. Returns the loaded
// instance, whether an attachment was found, and any error. Status,
// FailureMode, and PRURL at load time are snapshotted internally; subsequent
// Save calls diff against this snapshot to decide whether to persist and
// whether to append a History entry.
//
// When no attachment exists, Load returns a zero-valued PT with loaded=false.
// Callers can mutate it and Save to create the attachment.
func (m *StateManager[T, PT]) Load(ctx context.Context) (PT, bool, error) {
	var t T
	pt := PT(&t)
	loaded, err := m.sm.Load(ctx, pt)
	if err != nil {
		return pt, false, fmt.Errorf("load state: %w", err)
	}
	m.snapshotStatus = pt.GetStatus()
	m.snapshotFailureMode = pt.GetFailureMode()
	m.snapshotPRURL = pt.GetPRURL()
	// Clone slice snapshots so the diff baseline can't drift if a bot
	// mutates the State's slices in-place (vs. wholesale Set replacement).
	m.snapshotFindings = slices.Clone(pt.GetCurrentFindings())
	m.snapshotPendingChecks = slices.Clone(pt.GetCurrentPendingChecks())
	m.snapshotNoDiffCount = pt.GetNoDiffIterationCount()
	m.snapshotLinearType = pt.GetLinearStateType()
	m.snapshotLinearName = pt.GetLinearStateName()
	m.loaded = loaded
	return pt, loaded, nil
}

// Save persists the state, automatically appending a StateTransition to
// History when Status or FailureMode differs from the snapshot taken at
// Load. Returns (changed=true, nil) when the Linear attachment was
// actually written; callers can use this to gate Linear-side side-effects
// like one-time bot comments.
//
// Invariants enforced:
//
//   - History append on Status or FailureMode change. Actor + Trigger are
//     read from context (use metareconciler.WithActor / WithTrigger). A
//     missing actor or trigger results in empty fields on the entry —
//     silent and harmless, but the bot is responsible for setting them.
//   - FailureMode is cleared automatically when transitioning to a non-
//     StatusFailed status. Observability never sees Status=active alongside
//     a stale FailureMode.
//   - History is capped at historyCap entries with FIFO eviction. Downstream
//     mirrors are the place to keep unbounded transition history.
//   - The bot's BeforeSave hook fires before the dirty check; bots populate
//     wrapper-specific fields there. Returning true forces a Linear write
//     even when no framework field changed; returning false (the default)
//     keeps the wrapper mutation observability-only.
//   - Same-status, same-mode, same-PRURL, same-findings, same-pending Save
//     with BeforeSave returning false is a Linear-write no-op (returns
//     false, nil). Skipping the write avoids the feedback loop where a save
//     would trigger an attachment-update event that re-enqueues the issue
//     immediately. Findings/pending updates DO write (so the source-of-truth
//     attachment matches what downstream mirrors saw via the callback);
//     same-content saves of those fields are still no-ops.
//   - The post-save callback fires on every Save call regardless of whether
//     Linear was written, so downstream mirrors capture wrapper-specific
//     mutations from BeforeSave even when they don't transition the state
//     machine.
//
// The whole PT (including bot-specific fields embedded alongside State) is
// serialized; bot fields persist through framework Saves because they live
// on the wrapper type the manager is generic over.
func (m *StateManager[T, PT]) Save(ctx context.Context, pt PT) (bool, error) {
	currentStatus := pt.GetStatus()
	currentFailureMode := pt.GetFailureMode()

	// FailureMode invariant: clear on transition out of StatusFailed before
	// computing the diff, so the History entry's Mode field reflects the
	// post-clear value.
	if currentStatus != StatusFailed && currentFailureMode != "" {
		pt.SetFailureMode("")
		currentFailureMode = ""
	}

	// Bot pre-save hook: bots that shadow BeforeSave on their wrapper
	// populate wrapper-specific fields here (e.g. iteration markers from
	// trigger context). The default *State.BeforeSave is a no-op returning
	// false; bots that need to force a Linear write for their mutations
	// return true.
	botForcedDirty := pt.BeforeSave(ctx)

	// PRURL / findings / pending-checks are read after BeforeSave so the diff
	// against the snapshot uses post-hook values. Status/FailureMode aren't
	// re-read because BeforeSave is contracted to mutate only wrapper-
	// specific fields.
	currentPRURL := pt.GetPRURL()
	currentFindings := pt.GetCurrentFindings()
	currentPendingChecks := pt.GetCurrentPendingChecks()
	currentNoDiffCount := pt.GetNoDiffIterationCount()
	currentLinearType := pt.GetLinearStateType()
	currentLinearName := pt.GetLinearStateName()

	statusOrModeChanged := currentStatus != m.snapshotStatus || currentFailureMode != m.snapshotFailureMode
	prURLChanged := currentPRURL != m.snapshotPRURL
	findingsChanged := !slices.Equal(currentFindings, m.snapshotFindings)
	pendingChecksChanged := !slices.Equal(currentPendingChecks, m.snapshotPendingChecks)
	noDiffCountChanged := currentNoDiffCount != m.snapshotNoDiffCount
	linearStateChanged := currentLinearType != m.snapshotLinearType || currentLinearName != m.snapshotLinearName

	// Persist to Linear when any tracked field changed, when the bot opted
	// in via BeforeSave, OR when we never loaded (first save creates the
	// attachment). Findings/pending-checks changes also count: the Linear
	// attachment is the source of truth that the next reconcile's Load reads
	// from, so a downstream-only update (post-save callback fires but Linear
	// stays stale) would mean the next Load sees empty findings even though
	// the framework just observed CI activity. Keeping Linear in lockstep
	// with the post-save mirror prevents that divergence.
	//
	// PRURL-only and findings/pending-only changes save without a History
	// append — a from==to entry would mislead observability — but the
	// fields themselves must persist so consumers see current state.
	linearDirty := statusOrModeChanged || prURLChanged || findingsChanged || pendingChecksChanged || noDiffCountChanged || linearStateChanged || botForcedDirty || !m.loaded

	// pendingTransition is emitted as a CloudEvent only after the Linear
	// save below succeeds — an unpersisted transition (save failure) must
	// not reach observability as if it happened.
	var pendingTransition *StateTransitionEvent
	if statusOrModeChanged {
		actor, _ := ActorFromContext(ctx)
		trigger, _ := TriggerFromContext(ctx)
		at := m.now().UTC()
		pt.AppendHistory(StateTransition{
			From:    m.snapshotStatus,
			To:      currentStatus,
			At:      at,
			Actor:   actor,
			Trigger: trigger,
			Mode:    currentFailureMode,
		})
		if m.emitter != nil {
			pendingTransition = &StateTransitionEvent{
				Bot:             m.emitter.source,
				Provider:        stateTransitionProvider,
				IssueID:         m.issueID,
				IssueURL:        m.issueURL,
				PRURL:           currentPRURL,
				FromStatus:      m.snapshotStatus,
				ToStatus:        currentStatus,
				FailureMode:     currentFailureMode,
				Actor:           actor,
				Trigger:         trigger,
				LinearStateType: currentLinearType,
				LinearStateName: currentLinearName,
				TransitionAt:    at,
			}
		}
	}

	if linearDirty {
		// Inject framework-managed fields onto the bot's State pointer before
		// persisting. Setters are promoted from the embedded metareconciler.State
		// so bots never have to know about this; the framework guarantees the
		// persisted attachment carries IssueID/IssueURL on every save.
		pt.SetIssueID(m.issueID)
		pt.SetIssueURL(m.issueURL)

		if err := m.sm.Save(ctx, pt); err != nil {
			return false, fmt.Errorf("save state: %w", err)
		}

		clog.InfoContext(ctx, "Reconciler state saved",
			"from", m.snapshotStatus,
			"to", currentStatus,
			"changed", statusOrModeChanged,
		)
	}

	// Fire the post-save callback on every Save call, including no-ops
	// where the bot mutated wrapper fields without forcing a Linear write.
	// Downstream mirrors get a fresh state JSON every reconcile so iteration
	// markers (or any other bot-managed wrapper fields) reach observability
	// stores even when the state machine didn't transition.
	if m.cb != nil {
		stateJSON, err := json.Marshal(pt)
		if err != nil {
			clog.WarnContext(ctx, "Post-save callback skipped: marshal failed", "error", err)
		} else {
			m.cb(ctx, m.issueID, stateJSON)
		}
	}

	if pendingTransition != nil {
		m.emitter.emit(ctx, *pendingTransition)
	}

	if linearDirty {
		// Refresh the snapshot so a second Save in the same reconcile (rare,
		// but possible) doesn't re-append the same transition or re-flag the
		// PRURL/findings/pending as changed.
		m.snapshotStatus = currentStatus
		m.snapshotFailureMode = currentFailureMode
		m.snapshotPRURL = currentPRURL
		// Clone for the same reason as in Load — keep the diff baseline
		// independent of any in-place mutation a bot might do later.
		m.snapshotFindings = slices.Clone(currentFindings)
		m.snapshotPendingChecks = slices.Clone(currentPendingChecks)
		m.snapshotNoDiffCount = currentNoDiffCount
		m.snapshotLinearType = currentLinearType
		m.snapshotLinearName = currentLinearName
		m.loaded = true
	}

	return linearDirty, nil
}

// NotifyProgress fires the post-save callback with a fresh JSON snapshot of
// pt without writing the Linear attachment. Use this from inside long-running
// reconcile paths (e.g., before invoking a multi-minute agent run) to push
// in-flight state to downstream mirrors so post-save observers reflect that
// the bot is engaged rather than the previous reconcile's stale snapshot.
//
// Differences from Save:
//
//   - No Linear attachment write (avoids amplifying the duplicate-attachment
//     race in linearreconciler/state.go for what is fundamentally an
//     observability update).
//   - No History append (no state-machine transition occurred).
//   - Snapshot fields are NOT refreshed, so the next Save's dirty check still
//     compares against the load-time snapshot.
//   - BeforeSave IS invoked — bots that derive observability fields from
//     trigger context (LastIterationAt, Phase) get them populated on the
//     mirrored snapshot.
//
// No-op when no callback is configured. Marshal failures log a warning and
// return nil — the caller's primary work (the agent run) must not be blocked
// by a downstream mirror hiccup.
func (m *StateManager[T, PT]) NotifyProgress(ctx context.Context, pt PT) error {
	if m.cb == nil {
		return nil
	}
	pt.BeforeSave(ctx)
	stateJSON, err := json.Marshal(pt)
	if err != nil {
		clog.WarnContext(ctx, "NotifyProgress skipped: marshal failed", "error", err)
		return nil
	}
	m.cb(ctx, m.issueID, stateJSON)
	return nil
}

// UpsertBotComment passes through to the underlying StateManager. Exposed
// here so callers don't need to reach past StateManager[T, PT] to the
// linearreconciler.StateManager for non-state Linear operations.
func (m *StateManager[T, PT]) UpsertBotComment(ctx context.Context, body string) error {
	return m.sm.UpsertBotComment(ctx, body)
}

// UnprocessedComments passes through to the underlying StateManager.
func (m *StateManager[T, PT]) UnprocessedComments(botUserID string) []linearreconciler.Comment {
	return m.sm.UnprocessedComments(botUserID)
}
