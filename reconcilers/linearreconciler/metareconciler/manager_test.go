/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

const testActor = "test-bot"

// fixedClock returns a clock function that always returns the same time, so
// History entries' At fields are deterministic.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// loadSavedState parses the fixture's last-saved JSON into the given state
// pointer. Tests use it to inspect History and other fields directly rather
// than substring-matching on the JSON.
func loadSavedState(t *testing.T, f *linearStateFixture, into any) {
	t.Helper()
	raw := f.lastSavedState.Load()
	if raw == nil {
		t.Fatalf("no saved state captured")
	}
	if err := json.Unmarshal(*raw, into); err != nil {
		t.Fatalf("unmarshal saved state: %v", err)
	}
}

// newReconcilerForFixture builds a minimal Reconciler wired to the fixture's
// Linear client. State type defaults to the framework's State; tests that
// need bot-extension fields construct their own State type and parameterise
// directly.
func newReconcilerForFixture(t *testing.T, f *linearStateFixture) *Reconciler[testReq, testResp, testCB, State, *State] {
	t.Helper()
	return &Reconciler[testReq, testResp, testCB, State, *State]{
		identity:     testActor,
		linearClient: f.newClient(t),
	}
}

func TestStateManager_FromActiveToComplete(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	mgr.now = fixedClock(now)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true")
	}
	if got := f.saveCount.Load(); got != 1 {
		t.Fatalf("save count: got = %d, want = 1", got)
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.Status != StatusComplete {
		t.Errorf("Status: got = %q, want = %q", saved.Status, StatusComplete)
	}
	if got, want := len(saved.History), 1; got != want {
		t.Fatalf("History len: got = %d, want = %d", got, want)
	}
	entry := saved.History[0]
	if entry.From != StatusActive || entry.To != StatusComplete {
		t.Errorf("History[0] from/to: got = %q→%q, want = %q→%q", entry.From, entry.To, StatusActive, StatusComplete)
	}
	if entry.Actor != testActor || entry.Trigger != TriggerPRMerge {
		t.Errorf("History[0] actor/trigger: got = %q/%q, want = %q/%q", entry.Actor, entry.Trigger, testActor, TriggerPRMerge)
	}
	if !entry.At.Equal(now) {
		t.Errorf("History[0].At: got = %v, want = %v", entry.At, now)
	}
}

// TestStateManager_FromActiveToFailed covers the path the deleted
// markIssueFailed tests previously exercised: a status transition INTO
// StatusFailed with FailureMode set must persist both fields and append a
// History entry whose Mode field reflects the new classification.
func TestStateManager_FromActiveToFailed(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusFailed)
	s.SetFailureMode(FailureModePRClosed)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRClosed)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true")
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.Status != StatusFailed {
		t.Errorf("Status: got = %q, want = %q", saved.Status, StatusFailed)
	}
	if saved.FailureMode != FailureModePRClosed {
		t.Errorf("FailureMode: got = %q, want = %q", saved.FailureMode, FailureModePRClosed)
	}
	if got, want := len(saved.History), 1; got != want {
		t.Fatalf("History len: got = %d, want = %d", got, want)
	}
	if saved.History[0].Mode != FailureModePRClosed {
		t.Errorf("History[0].Mode: got = %q, want = %q", saved.History[0].Mode, FailureModePRClosed)
	}
}

// TestStateManager_FromActiveToFailedNoDiff covers the no_diff terminal
// path added for the empty-commit-retry-loop bug: when the agent runs
// but produces no changes, the framework transitions State to
// StatusFailed + FailureModeNoDiff. The structured FailureMode is what
// downstream consumers match on to surface the "agent ran but had
// nothing to do" outcome instead of a stuck-active retry storm.
func TestStateManager_FromActiveToFailedNoDiff(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusFailed)
	s.SetFailureMode(FailureModeNoDiff)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerNoDiff)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true")
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.Status != StatusFailed {
		t.Errorf("Status: got = %q, want = %q", saved.Status, StatusFailed)
	}
	if saved.FailureMode != FailureModeNoDiff {
		t.Errorf("FailureMode: got = %q, want = %q", saved.FailureMode, FailureModeNoDiff)
	}
	if got, want := len(saved.History), 1; got != want {
		t.Fatalf("History len: got = %d, want = %d", got, want)
	}
	entry := saved.History[0]
	if entry.From != StatusActive || entry.To != StatusFailed {
		t.Errorf("History[0] from/to: got = %q→%q, want = %q→%q", entry.From, entry.To, StatusActive, StatusFailed)
	}
	if entry.Trigger != TriggerNoDiff {
		t.Errorf("History[0].Trigger: got = %q, want = %q", entry.Trigger, TriggerNoDiff)
	}
	if entry.Mode != FailureModeNoDiff {
		t.Errorf("History[0].Mode: got = %q, want = %q", entry.Mode, FailureModeNoDiff)
	}
}

func TestStateManager_NoOpWhenSameState(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"complete"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No mutations — Save should be a no-op.
	changed, err := mgr.Save(t.Context(), s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if changed {
		t.Errorf("changed: got = true, want = false (no-op call must not save)")
	}
	if got := f.saveCount.Load(); got != 0 {
		t.Errorf("save count: got = %d, want = 0", got)
	}
}

// TestStateManager_ClearsFailureModeOnRecovery verifies the FailureMode
// invariant: when a previously-failed issue transitions back to a non-failed
// status, FailureMode is cleared so observability never sees an active issue
// with a stale failure classification.
func TestStateManager_ClearsFailureModeOnRecovery(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"failed","failure_mode":"max_turns"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusActive)
	// Note: caller did NOT clear FailureMode — Save must do it automatically.

	ctx := WithTrigger(t.Context(), TriggerMergeConflict)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true")
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.Status != StatusActive {
		t.Errorf("Status: got = %q, want = %q", saved.Status, StatusActive)
	}
	if saved.FailureMode != "" {
		t.Errorf("FailureMode: got = %q, want = \"\" (must clear on transition out of StatusFailed)", saved.FailureMode)
	}
	// Manager clears FailureMode BEFORE the History append, so the entry's
	// Mode field must reflect the post-clear value. Asserting this turns an
	// implicit invariant into a test-driven one.
	if got, want := len(saved.History), 1; got != want {
		t.Fatalf("History len: got = %d, want = %d", got, want)
	}
	if saved.History[0].Mode != "" {
		t.Errorf("History[0].Mode: got = %q, want = \"\" (cleared before append)", saved.History[0].Mode)
	}
}

// TestStateManager_PRURLOnlyChangePersists is the regression test for the
// dirty predicate covering PRURL changes. Prior to including PRURL in the
// snapshot, a same-status reconcile with a different prURL would return
// (false, nil) and silently drop the new URL — leaving stale data on the
// Linear issue and breaking the bot-comment gate that depends on changed=true.
func TestStateManager_PRURLOnlyChangePersists(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Status unchanged; only PRURL changes (e.g. PR was recreated, or Upsert
	// returned a refreshed URL).
	const newPRURL = "https://github.com/o/r/pull/2"
	s.SetPRURL(newPRURL)

	changed, err := mgr.Save(t.Context(), s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true (PRURL change must persist)")
	}
	if got := f.saveCount.Load(); got != 1 {
		t.Errorf("save count: got = %d, want = 1", got)
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.PRURL != newPRURL {
		t.Errorf("PRURL: got = %q, want = %q", saved.PRURL, newPRURL)
	}
	// PRURL-only changes must NOT append a History entry — a from==to entry
	// would mislead observability. The save fires; the audit trail stays clean.
	if got, want := len(saved.History), 0; got != want {
		t.Errorf("History len: got = %d, want = %d (PRURL-only change must not append History)", got, want)
	}
}

// TestStateManager_FailureModeReclassification verifies a Failed→Failed
// transition with a different FailureMode persists the new mode.
func TestStateManager_FailureModeReclassification(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"failed","failure_mode":"max_turns"}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetFailureMode(FailureModePRClosed) // reclassify

	changed, err := mgr.Save(t.Context(), s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true (mode change must persist)")
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.FailureMode != FailureModePRClosed {
		t.Errorf("FailureMode: got = %q, want = %q", saved.FailureMode, FailureModePRClosed)
	}
	if got, want := len(saved.History), 1; got != want {
		t.Fatalf("History len: got = %d, want = %d", got, want)
	}
	if saved.History[0].Mode != FailureModePRClosed {
		t.Errorf("History[0].Mode: got = %q, want = %q", saved.History[0].Mode, FailureModePRClosed)
	}
}

// TestStateManager_HistoryAccumulates verifies that successive transitions
// build up a chronological History rather than overwriting prior entries.
func TestStateManager_HistoryAccumulates(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"active","history":[{"to":"active","at":"2026-04-29T11:00:00Z","actor":"test-bot","trigger":"initial_run"}]}`)
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatalf("changed: got = false, want = true")
	}

	var saved State
	loadSavedState(t, f, &saved)
	if got, want := len(saved.History), 2; got != want {
		t.Fatalf("History len: got = %d, want = %d (must append, not replace)", got, want)
	}
	if saved.History[0].Trigger != TriggerInitialRun {
		t.Errorf("History[0].Trigger: got = %q, want = %q (existing entry preserved)", saved.History[0].Trigger, TriggerInitialRun)
	}
	if saved.History[1].Trigger != TriggerPRMerge {
		t.Errorf("History[1].Trigger: got = %q, want = %q (new entry appended)", saved.History[1].Trigger, TriggerPRMerge)
	}
}

// TestStateManager_HistoryCapFIFOEvicts verifies that History stops growing
// past historyCap, with oldest entries dropped first.
func TestStateManager_HistoryCapFIFOEvicts(t *testing.T) {
	// Pre-fill state with historyCap entries so the next append triggers eviction.
	s := &State{}
	s.History = make([]StateTransition, historyCap)
	for i := range s.History {
		s.History[i] = StateTransition{To: StatusActive, Trigger: "fill", At: time.Unix(int64(i), 0).UTC()}
	}
	// One more append should evict the oldest.
	s.AppendHistory(StateTransition{To: StatusComplete, Trigger: "after-cap", At: time.Unix(int64(historyCap), 0).UTC()})

	if got, want := len(s.History), historyCap; got != want {
		t.Errorf("History len: got = %d, want = %d (cap should hold)", got, want)
	}
	if s.History[len(s.History)-1].Trigger != "after-cap" {
		t.Errorf("History tail: got = %q, want = %q", s.History[len(s.History)-1].Trigger, "after-cap")
	}
	if s.History[0].At.Unix() != 1 {
		t.Errorf("oldest entry should have been evicted: got first At = %v", s.History[0].At)
	}
}

// extendedState is a synthetic bot-side wrapper used to verify that bot-private
// fields round-trip through framework Save/Load unchanged. Mirrors what a real
// bot would do in its `internal/state` package.
type extendedState struct {
	State
	BotField string `json:"bot_field,omitempty"`
}

// TestStateManager_PreservesBotFields verifies the load-bearing claim that
// generic StateManager preserves bot-specific fields across framework writes.
// If this test ever fails, the embedding pattern is broken and bots will lose
// data on every reconcile.
func TestStateManager_PreservesBotFields(t *testing.T) {
	f := newLinearStateFixture(t, `{"pr_url":"https://github.com/o/r/pull/1","status":"active","bot_field":"keep-me"}`)

	// Construct a Reconciler over the bot's wrapper type.
	r := &Reconciler[testReq, testResp, testCB, extendedState, *extendedState]{
		identity:     testActor,
		linearClient: f.newClient(t),
	}
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.BotField != "keep-me" {
		t.Fatalf("bot field on Load: got = %q, want = %q", s.BotField, "keep-me")
	}
	s.SetStatus(StatusComplete) // framework-side mutation

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	if _, err := mgr.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var saved extendedState
	loadSavedState(t, f, &saved)
	if saved.BotField != "keep-me" {
		t.Errorf("bot field after framework Save: got = %q, want = %q (bot fields must round-trip)", saved.BotField, "keep-me")
	}
	if saved.Status != StatusComplete {
		t.Errorf("Status: got = %q, want = %q", saved.Status, StatusComplete)
	}
}

// TestStateManager_InjectsIssueIDAndURLOnSave verifies that StateManager
// captures issue.ID and issue.URL at construction and writes them into the
// persisted state on every save, regardless of whether the bot touched
// them. Bots get a self-contained attachment for free.
func TestStateManager_InjectsIssueIDAndURLOnSave(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"active"}`)
	f.issue.URL = "https://linear.app/example/issue/ENG-1"
	r := newReconcilerForFixture(t, f)
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	if _, err := mgr.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var saved State
	loadSavedState(t, f, &saved)
	if saved.IssueID != f.issue.ID {
		t.Errorf("IssueID: got = %q, want = %q", saved.IssueID, f.issue.ID)
	}
	if saved.IssueURL != f.issue.URL {
		t.Errorf("IssueURL: got = %q, want = %q", saved.IssueURL, f.issue.URL)
	}
}

// TestStateManager_SaveCallback_FiresWithStateJSONAndIssueID covers the
// post-save hook surface that downstream consumers plug into. The callback
// must fire on every successful save, receive the linear issue ID, and the
// marshaled bot State (post-Set of framework-managed IssueID/IssueURL).
func TestStateManager_SaveCallback_FiresWithStateJSONAndIssueID(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"active"}`)
	f.issue.URL = "https://linear.app/example/issue/ENG-1"
	r := newReconcilerForFixture(t, f)

	type capture struct {
		issueID string
		raw     []byte
		fired   int
	}
	var got capture
	mgr := r.NewStateManager(f.issue).SetSaveCallback(func(_ context.Context, issueID string, stateJSON []byte) {
		got.issueID = issueID
		got.raw = stateJSON
		got.fired++
	})

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.SetStatus(StatusComplete)

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerPRMerge)
	if _, err := mgr.Save(ctx, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got.fired != 1 {
		t.Fatalf("callback fired: got = %d, want = 1", got.fired)
	}
	if got.issueID != f.issue.ID {
		t.Errorf("callback issueID: got = %q, want = %q", got.issueID, f.issue.ID)
	}

	var captured State
	if err := json.Unmarshal(got.raw, &captured); err != nil {
		t.Fatalf("unmarshal callback stateJSON: %v", err)
	}
	if captured.IssueID != f.issue.ID {
		t.Errorf("captured IssueID: got = %q, want = %q", captured.IssueID, f.issue.ID)
	}
	if captured.IssueURL != f.issue.URL {
		t.Errorf("captured IssueURL: got = %q, want = %q", captured.IssueURL, f.issue.URL)
	}
	if captured.Status != StatusComplete {
		t.Errorf("captured Status: got = %q, want = %q", captured.Status, StatusComplete)
	}
}

// TestStateManager_SaveCallback_FiresOnNoOpSave verifies that the post-save
// callback fires even when the framework skips the Linear write. Downstream
// mirrors need to see every Save call so wrapper-specific fields populated
// by a bot's BeforeSave hook (e.g. iteration markers) reach observability
// stores even when the state machine didn't transition. The Linear save is
// still gated by the dirty check — the callback fire is independent.
func TestStateManager_SaveCallback_FiresOnNoOpSave(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"complete","pr_url":"https://github.com/o/r/pull/1"}`)
	r := newReconcilerForFixture(t, f)

	var fired int
	mgr := r.NewStateManager(f.issue).SetSaveCallback(func(_ context.Context, _ string, _ []byte) {
		fired++
	})

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No mutation — Linear write is skipped (changed=false), but callback
	// fires anyway so the downstream mirror sees a fresh state JSON.
	changed, err := mgr.Save(t.Context(), s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if changed {
		t.Errorf("changed: got = true, want = false (no field changed, Linear write should be skipped)")
	}
	if got := f.saveCount.Load(); got != 0 {
		t.Errorf("save count: got = %d, want = 0 (no Linear write expected)", got)
	}
	if fired != 1 {
		t.Errorf("callback fired: got = %d, want = 1 (callback must fire even on no-op save)", fired)
	}
}

// stateWithBeforeSave is a synthetic bot wrapper that exercises the
// BeforeSave shadow pattern. callCount tracks invocations; forceSave
// controls the return value. BotField is populated from the trigger
// context so the test can assert that BeforeSave's mutations are visible
// in the callback's stateJSON.
type stateWithBeforeSave struct {
	State
	BotField string `json:"bot_field,omitempty"`

	callCount int
	forceSave bool
}

func (s *stateWithBeforeSave) BeforeSave(ctx context.Context) bool {
	s.callCount++
	if trigger, _ := TriggerFromContext(ctx); trigger != "" {
		s.BotField = "set-by-before-save:" + trigger
	}
	return s.forceSave
}

// TestStateManager_BeforeSave_CalledOnEveryNoOpSave verifies the bot's
// shadowed BeforeSave runs even when nothing else triggers a save, and
// that the wrapper-field mutations it makes reach the post-save callback
// even though Linear isn't written.
func TestStateManager_BeforeSave_CalledOnEveryNoOpSave(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"active","pr_url":"https://github.com/o/r/pull/1"}`)
	r := &Reconciler[testReq, testResp, testCB, stateWithBeforeSave, *stateWithBeforeSave]{
		identity:     testActor,
		linearClient: f.newClient(t),
	}

	var captured []byte
	mgr := r.NewStateManager(f.issue).SetSaveCallback(func(_ context.Context, _ string, stateJSON []byte) {
		captured = stateJSON
	})

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerCIFailureIteration)

	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if changed {
		t.Errorf("changed: got = true, want = false (BeforeSave returns false; no Linear write expected)")
	}
	if got := f.saveCount.Load(); got != 0 {
		t.Errorf("save count: got = %d, want = 0", got)
	}
	if s.callCount != 1 {
		t.Errorf("BeforeSave callCount: got = %d, want = 1", s.callCount)
	}

	var seen stateWithBeforeSave
	if err := json.Unmarshal(captured, &seen); err != nil {
		t.Fatalf("unmarshal callback stateJSON: %v", err)
	}
	if want := "set-by-before-save:" + TriggerCIFailureIteration; seen.BotField != want {
		t.Errorf("callback saw BotField: got = %q, want = %q (BeforeSave mutations must reach the callback)", seen.BotField, want)
	}
}

// TestStateManager_BeforeSave_ForceSaveTriggersLinear verifies that a bot
// that returns true from BeforeSave forces a Linear write even when no
// framework-tracked field changed. The escape hatch lets bots persist
// wrapper-specific changes durably to the attachment when needed.
func TestStateManager_BeforeSave_ForceSaveTriggersLinear(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"active","pr_url":"https://github.com/o/r/pull/1"}`)
	r := &Reconciler[testReq, testResp, testCB, stateWithBeforeSave, *stateWithBeforeSave]{
		identity:     testActor,
		linearClient: f.newClient(t),
	}
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.forceSave = true

	ctx := WithActor(t.Context(), testActor)
	ctx = WithTrigger(ctx, TriggerCIFailureIteration)

	changed, err := mgr.Save(ctx, s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Errorf("changed: got = false, want = true (BeforeSave returned true; Linear write expected)")
	}
	if got := f.saveCount.Load(); got != 1 {
		t.Errorf("save count: got = %d, want = 1", got)
	}

	var saved stateWithBeforeSave
	loadSavedState(t, f, &saved)
	if want := "set-by-before-save:" + TriggerCIFailureIteration; saved.BotField != want {
		t.Errorf("saved BotField: got = %q, want = %q", saved.BotField, want)
	}
	// Status didn't change, so no History entry should be appended even
	// though the Linear write fired — the diff-based history append is
	// independent of the Linear-write decision.
	if got := len(saved.History); got != 0 {
		t.Errorf("History len: got = %d, want = 0 (BeforeSave force-save must not fabricate transitions)", got)
	}
}

// TestStateManager_BeforeSave_DefaultIsNoOp verifies that a bot wrapper
// that does not shadow BeforeSave inherits the framework's no-op default
// via embedding. This is the load-bearing claim for the DX promise that
// future bots get the hook for free without implementing anything.
func TestStateManager_BeforeSave_DefaultIsNoOp(t *testing.T) {
	f := newLinearStateFixture(t, `{"status":"complete","pr_url":"https://github.com/o/r/pull/1"}`)
	// extendedState (defined above) does NOT shadow BeforeSave, so it
	// inherits the *State.BeforeSave default returning false.
	r := &Reconciler[testReq, testResp, testCB, extendedState, *extendedState]{
		identity:     testActor,
		linearClient: f.newClient(t),
	}
	mgr := r.NewStateManager(f.issue)

	s, _, err := mgr.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No mutation, no shadowed BeforeSave → no Linear write.
	changed, err := mgr.Save(t.Context(), s)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if changed {
		t.Errorf("changed: got = true, want = false (default BeforeSave must not force-save)")
	}
}
