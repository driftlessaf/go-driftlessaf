/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/workqueue"
)

const (
	oosConstraint = "Do not edit the README."
	oosBody       = "Please do the thing.\n\n## Constraints\n\n" + oosConstraint + "\n"
	oosFinding    = "Please update the README."
	oosBlob       = "sha1"
)

// oosFixture is an open PR for an issue that forbids README edits, with a fake
// contents API for the evidence file. The PR body is the only place the
// decision is kept between reconciles.
type oosFixture struct {
	t          *testing.T
	h          *issueHarness
	f          *steeringFake
	blob       string // content.txt's blob SHA; empty answers 404. Guarded by h.mu.
	blobStatus int    // nonzero returns this status from the contents API. Guarded by h.mu.
	blobReads  int
	pathStatus map[string]int // per-path status overriding blobStatus. Guarded by h.mu.
	decisions  []OutOfScopeDecision
	checks     *checkFake
}

func (fx *oosFixture) opt() Option[*testRequest, *publishResult, struct{}] {
	return func(r *Reconciler[*testRequest, *publishResult, struct{}]) {
		WithOutOfScopeDispositions[*testRequest, *publishResult, struct{}](func(*publishResult) []OutOfScopeDecision { return fx.decisions })(r)
		if fx.checks != nil {
			withEvidenceCheck()(r)
		}
	}
}

func newOOSFixture(t *testing.T) *oosFixture {
	t.Helper()
	fx := &oosFixture{t: t, blob: oosBlob}
	fx.h, fx.f = steeringHarness(t, oosBody, fx.opt())
	fx.h.extra = fx.serveExtra
	fx.decisions = []OutOfScopeDecision{{Identifier: "review-body:1", Constraint: oosConstraint, Evidence: []string{"content.txt"}, WholeReview: true}}
	return fx
}

func (fx *oosFixture) serveExtra(w http.ResponseWriter, r *http.Request) bool {
	if fx.checks != nil && fx.checks.serve(w, r) {
		return true
	}
	if fx.f.serve(w, r) {
		return true
	}
	p, ok := strings.CutPrefix(r.URL.Path, "/repos/owner/repo/contents/")
	if !ok || r.Method != http.MethodGet {
		return false
	}
	fx.blobReads++
	status := fx.blobStatus
	if s, ok := fx.pathStatus[p]; ok {
		status = s
	}
	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{"message":"temporary failure"}`))
		return true
	}
	if fx.blob == "" {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
		return true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "name": p, "path": p, "sha": fx.blob, "encoding": "base64", "content": ""})
	return true
}

func (fx *oosFixture) setReviews(rs ...harnessReview) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.h.reviews = rs
}

func (fx *oosFixture) setBlob(sha string) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.blob = sha
}

func (fx *oosFixture) setBlobStatus(status int) {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	fx.blobStatus = status
}

func (fx *oosFixture) stored() bool {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	return strings.Contains(fx.h.prText, `"out_of_scope"`)
}

func bot(id int64, body string) harnessReview {
	return harnessReview{id: id, login: "reviewer[bot]", association: "NONE", body: body}
}

// first runs the reconcile in which the agent declines the review.
func (fx *oosFixture) first(body string) {
	fx.t.Helper()
	fx.setReviews(bot(1, body))
	fx.h.reconcile(fx.t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(fx.t, "first run", 1)
}

// TestOutOfScopeDecisionPersistsAcrossReconciles runs two reconciles through
// the PR's body: the first stores the decision, and a fresh review ID with the
// same text reuses it with no agent run.
func TestOutOfScopeDecisionPersistsAcrossReconciles(t *testing.T) {
	fx := newOOSFixture(t)
	fx.first(oosFinding)
	if !fx.stored() {
		t.Fatalf("PR body after the first run: got = %q, want an out_of_scope record", fx.h.prText)
	}

	notesBefore := fx.h.prNotes()
	fx.h.addComment(viewerLogin, giveUpMarker+"\nstale source issue note")
	fx.setReviews(bot(2, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "repeat after an unrelated push", 1)
	if n := len(fx.h.notes()); n != 0 {
		t.Errorf("source issue notes after reuse: got = %d, want = 0", n)
	}
	if notesAfter := fx.h.prNotes(); !slices.Equal(notesAfter, notesBefore) {
		t.Errorf("PR notes after reuse: got = %q, want = %q (the give-up note stays)", notesAfter, notesBefore)
	}
	if fx.h.prCreates != 1 {
		t.Errorf("PRs created: got = %d, want = 1", fx.h.prCreates)
	}
}

func TestDecisionRequiresLiveBodyProvenance(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		fail   bool
	}{
		{name: "different PR body", fields: map[string]any{"body": "edited after the session read"}},
		{name: "new head", fields: map[string]any{"headRefOid": "later"}},
		{name: "other viewer", fields: map[string]any{"viewerDidAuthor": false}},
		{name: "human creator", fields: map[string]any{"author": map[string]any{"__typename": "User"}}},
		{name: "unavailable edit history", fields: map[string]any{"userContentEdits": nil}},
		{name: "redacted edit", fields: map[string]any{"userContentEdits": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"deletedAt": "2026-01-01T00:00:00Z", "editor": map[string]any{"id": "BOT_fixture", "__typename": "Bot"}}}}}},
		{name: "different editing app", fields: map[string]any{"userContentEdits": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"editor": map[string]any{"__typename": "Bot", "id": "BOT_other"}}}}}},
		{name: "history count mismatch", fields: map[string]any{"userContentEdits": map[string]any{"totalCount": 2, "nodes": []any{}}}},
		{name: "query error", fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newOOSFixture(t)
			fx.first(oosFinding)
			fx.h.provenanceFields = tc.fields
			fx.h.provenanceFails = tc.fail
			fx.setReviews(bot(2, oosFinding))
			fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
			fx.h.wantRun(t, tc.name, 2)
		})
	}
}

func TestOutOfScopeDecisionReevaluated(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(fx *oosFixture)
		noOption bool
	}{{
		name: "incomplete PR edit history",
		setup: func(fx *oosFixture) {
			fx.h.moreBodyEdits = true
			fx.setReviews(bot(2, oosFinding))
		},
	}, {
		name: "a bot rewrite after a human edited the PR body",
		setup: func(fx *oosFixture) {
			fx.h.priorBodyEditor = "teammate"
			fx.setReviews(bot(2, oosFinding))
		},
	}, {
		name:  "a distinct concern in the same review",
		setup: func(fx *oosFixture) { fx.setReviews(bot(2, oosFinding+"\nAlso fix the race in the cache.")) },
	}, {
		name:  "a different case",
		setup: func(fx *oosFixture) { fx.setReviews(bot(2, strings.ToLower(oosFinding))) },
	}, {
		name: "a human with the same text",
		setup: func(fx *oosFixture) {
			fx.setReviews(harnessReview{id: 2, login: "alice", association: "OWNER", body: oosFinding})
		},
	}, {
		name:  "changed evidence",
		setup: func(fx *oosFixture) { fx.setBlob("sha2"); fx.setReviews(bot(2, oosFinding)) },
	}, {
		name:  "missing evidence",
		setup: func(fx *oosFixture) { fx.setBlob(""); fx.setReviews(bot(2, oosFinding)) },
	}, {
		name: "new steering in the issue",
		setup: func(fx *oosFixture) {
			fx.h.setIssueBody(oosBody + "\n## Steering\n\nREADME edits are fine.\nSteering-ID: a\n")
			fx.setReviews(bot(2, oosFinding))
		},
	}, {
		name:  "a failing check",
		setup: func(fx *oosFixture) { fx.h.setPR("MERGEABLE", failingCheck); fx.setReviews(bot(2, oosFinding)) },
	}, {
		name:     "the option off",
		setup:    func(fx *oosFixture) { fx.setReviews(bot(2, oosFinding)) },
		noOption: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOOSFixture(t)
			fx.first(oosFinding)
			tt.setup(fx)
			if tt.noOption {
				fx.h.reconcile(t, noChange, withGiveUp())
			} else {
				fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
			}
			fx.h.wantRun(t, tt.name, 2)
		})
	}
}

// TestOutOfScopeScopeLabel proves adding the workflow-edit label, which widens
// the accepted scope, sends a declined review to the agent again.
func TestOutOfScopeScopeLabel(t *testing.T) {
	fx := newOOSFixture(t)
	fx.first(oosFinding)
	fx.h.setLabels(requiredLabel, workflowEditLabel)
	fx.setReviews(bot(2, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt(), WithWorkflowEditLabel[*testRequest, *publishResult, struct{}](workflowEditLabel))
	fx.h.wantRun(t, "label added", 2)
}

func TestOutOfScopeEvidenceCheckReconsidersInvalidatedDecision(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	tests := []struct {
		name   string
		change func(*oosFixture)
	}{
		{
			name: "accepted scope changed",
			change: func(fx *oosFixture) {
				fx.h.mu.Lock()
				fx.h.issue.Title = new("A different accepted request")
				fx.h.mu.Unlock()
			},
		},
		{
			name:   "cited evidence changed",
			change: func(fx *oosFixture) { fx.setBlob("sha2") },
		},
		{
			name:   "cited evidence deleted",
			change: func(fx *oosFixture) { fx.setBlob("") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOOSFixture(t)
			fx.checks = &checkFake{appID: testCheckApp}
			fx.setReviews(bot(1, oosFinding))
			fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
			fx.h.wantRun(t, "first no-op", 1)
			if !fx.stored() {
				t.Fatal("first no-op did not persist its out-of-scope decision")
			}

			tt.change(fx)
			fx.decisions = nil
			fx.setReviews(bot(1, oosFinding))
			fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
			fx.h.wantRun(t, "same review after invalidation", 2)
			if fx.stored() {
				t.Fatal("invalidated decision remained stored after reconsideration")
			}

			fx.setReviews(bot(1, oosFinding))
			fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
			fx.h.wantRun(t, "unchanged review after reconsideration", 2)
		})
	}
}

func TestNewScopeWithIncompleteBodyHistoryRunsOnce(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.first(oosFinding)
	fx.h.mu.Lock()
	fx.h.moreBodyEdits = true
	fx.h.issue.Title = new("A different accepted request")
	fx.h.mu.Unlock()
	for range 2 {
		fx.setReviews(bot(1, oosFinding))
		fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
		fx.h.wantRun(t, "changed title with incomplete body history", 2)
	}
}

func TestOutOfScopeEvidenceReadFailureReevaluatesWithoutInvalidatingDecision(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.setReviews(bot(1, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "initial decision", 1)
	if !fx.stored() {
		t.Fatal("initial out-of-scope decision was not stored")
	}

	fx.setBlobStatus(http.StatusServiceUnavailable)
	fx.setReviews(bot(1, oosFinding))
	if err := fx.h.reconcileErr(t, t.Context(), noChange, withGiveUp(), fx.opt()); err != nil {
		t.Fatalf("reconcile with unavailable evidence: %v", err)
	}
	fx.h.wantRun(t, "transient evidence read failure", 1)
	if !fx.stored() {
		t.Fatal("transient evidence read failure invalidated the stored decision")
	}

	// A persistent outage must not bypass the attempt record and run the agent
	// again for the same head, intent and findings.
	fx.setReviews(bot(1, oosFinding))
	if err := fx.h.reconcileErr(t, t.Context(), noChange, withGiveUp(), fx.opt()); err != nil {
		t.Fatalf("second reconcile with unavailable evidence: %v", err)
	}
	fx.h.wantRun(t, "repeated transient evidence read failure", 1)
	if !fx.stored() {
		t.Fatal("repeated transient evidence read failure invalidated the stored decision")
	}

	// Only a definitive missing-file response invalidates the evidence and
	// permits one reconsideration of this unchanged review.
	fx.setBlobStatus(http.StatusNotFound)
	fx.setReviews(bot(1, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "confirmed missing evidence", 2)
	if fx.stored() {
		t.Fatal("confirmed missing evidence kept the stale disposition")
	}

	fx.setReviews(bot(1, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "unchanged review after invalidation", 2)
}

// TestOutOfScopeIndependentEvidenceReadOutcomes proves two stored decisions are
// judged independently in one reconcile: a transient read failure keeps the
// first, a definitive 404 removes the second, the same review is reconsidered
// once, and the next identical reconcile dedupes.
func TestOutOfScopeIndependentEvidenceReadOutcomes(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	const second = "Please update the README again."
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.decisions = []OutOfScopeDecision{
		{Identifier: "review-body:1", Constraint: oosConstraint, Evidence: []string{"a.txt"}, WholeReview: true},
		{Identifier: "review-body:2", Constraint: oosConstraint, Evidence: []string{"b.txt"}, WholeReview: true},
	}
	fx.setReviews(bot(1, oosFinding), bot(2, second))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "initial decisions", 1)
	if n := fx.records(); n != 2 {
		t.Fatalf("stored records after the first run: got = %d, want = 2", n)
	}

	fx.h.mu.Lock()
	fx.pathStatus = map[string]int{"a.txt": http.StatusServiceUnavailable, "b.txt": http.StatusNotFound}
	fx.h.mu.Unlock()
	fx.decisions = nil
	fx.setReviews(bot(1, oosFinding), bot(2, second))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "mixed evidence read outcomes", 2)
	if n := fx.records(); n != 1 {
		t.Errorf("stored records after mixed outcomes: got = %d, want = 1 (the 503 record stays, the 404 record is removed)", n)
	}

	fx.setReviews(bot(1, oosFinding), bot(2, second))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "unchanged reviews after reconsideration", 2)
	if n := fx.records(); n != 1 {
		t.Errorf("stored records after the repeat: got = %d, want = 1", n)
	}
}

// TestOutOfScopeLongReviewAttemptDedupes proves a review cut at the display cap
// still gets per-head attempt dedupe when the evidence check is on.
func TestOutOfScopeLongReviewAttemptDedupes(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	long := oosFinding + " " + strings.Repeat("x", 5000)
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.setReviews(bot(1, long))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "first run", 1)

	fx.setReviews(bot(1, long))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "same long review on the same head", 1)

	fx.setReviews(bot(1, long+"\nAlso a new concern after the cutoff."))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "changed tail of the long review", 2)
}

// records counts the out-of-scope records stored in the PR body.
func (fx *oosFixture) records() int {
	fx.h.mu.Lock()
	defer fx.h.mu.Unlock()
	return strings.Count(fx.h.prText, `"fingerprint"`)
}

// TestOutOfScopeEvidenceOutageDoesNotSuppressNewIntent proves a transient
// evidence outage does not suppress a new accepted intent revision, which has
// no recorded attempt: it reaches evaluation exactly once and then dedupes.
func TestOutOfScopeEvidenceOutageDoesNotSuppressNewIntent(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.setReviews(bot(1, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "initial decision", 1)
	if !fx.stored() {
		t.Fatal("initial out-of-scope decision was not stored")
	}

	fx.setBlobStatus(http.StatusServiceUnavailable)
	fx.h.mu.Lock()
	fx.h.issue.Title = new("A different accepted request")
	fx.h.mu.Unlock()
	for range 2 {
		fx.setReviews(bot(1, oosFinding))
		if err := fx.h.reconcileErr(t, t.Context(), noChange, withGiveUp(), fx.opt()); err != nil {
			t.Fatalf("reconcile with unavailable evidence: %v", err)
		}
		fx.h.wantRun(t, "new intent during evidence outage", 2)
	}
}

func TestOutOfScopeEvidenceCheckDoesNotWriteAfterConcurrentPRBodyChange(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	fx := newOOSFixture(t)
	fx.checks = &checkFake{appID: testCheckApp}
	fx.setReviews(bot(1, oosFinding))
	fx.h.reconcile(t, func(context.Context) error {
		fx.h.mu.Lock()
		fx.h.prText += "\nHuman edit while the run was active.\n"
		fx.h.mu.Unlock()
		return nil
	}, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "no-op with a concurrent body edit", 1)
	fx.h.mu.Lock()
	gotBody := fx.h.prText
	fx.h.mu.Unlock()
	if !strings.Contains(gotBody, "Human edit while the run was active.") {
		t.Errorf("concurrent PR body edit was lost: %q", gotBody)
	}
	if strings.Contains(gotBody, `"out_of_scope"`) {
		t.Errorf("stale out-of-scope decision was persisted into the concurrent edit: %q", gotBody)
	}
}

// TestOutOfScopeNotWholeReview proves a decision that does not cover the whole
// review is not stored.
func TestOutOfScopeNotWholeReview(t *testing.T) {
	fx := newOOSFixture(t)
	fx.decisions[0].WholeReview = false
	fx.first(oosFinding)
	if fx.stored() {
		t.Error("out_of_scope record stored for a partial decision")
	}
	fx.setReviews(bot(2, oosFinding))
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "repeat", 2)
}

// TestOutOfScopeRebaseNotRetained proves a run leased on the default branch
// because the PR needs a rebase records nothing: it did not read the code at
// the recorded PR head.
func TestOutOfScopeRebaseNotRetained(t *testing.T) {
	fx := newOOSFixture(t)
	fx.h.setPR("CONFLICTING", "[]")
	fx.first(oosFinding)
	if fx.stored() {
		t.Errorf("stored = %v, want = false (PR body %q)", true, fx.h.prText)
	}
}

// TestOutOfScopeTruncatedReviewNotRetained proves a review the renderer cuts at
// the display cap is never stored or reused, so a concern after the cut reaches
// the agent.
func TestOutOfScopeTruncatedReviewNotRetained(t *testing.T) {
	long := oosFinding + " " + strings.Repeat("x", 5000)
	fx := newOOSFixture(t)
	fx.first(long)
	if fx.stored() {
		t.Error("out_of_scope record stored for a truncated review")
	}
	fx.h.fullReviewBodyLogs = true
	const concern = "Also a new security concern."
	fx.setReviews(bot(2, long+"\n"+concern))
	var read strings.Builder
	var readErr error
	reader := func(ctx context.Context) error {
		fx.h.mu.Lock()
		get := fx.h.findingCbs.GetLogs
		fx.h.mu.Unlock()
		if get == nil {
			readErr = errors.New("no finding callbacks")
			return nil
		}
		out, err := get(ctx, callbacks.FindingKindReview, "review-body:2")
		if err != nil {
			readErr = err
			return nil
		}
		read.WriteString(out)
		return nil
	}
	fx.h.reconcile(t, reader, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "concern after the cutoff", 2)
	if readErr != nil {
		t.Fatalf("GetDetails: %v", readErr)
	}
	if !strings.Contains(read.String(), concern) {
		t.Errorf("agent read: got = %q, want = text containing %q", read.String(), concern)
	}
}

// TestOutOfScopeEvidenceChangedDuringRunNotRetained proves a decision citing a
// file the run modified is not stored, since the committed blob may differ from
// what the agent read.
func TestOutOfScopeEvidenceChangedDuringRunNotRetained(t *testing.T) {
	fx := newOOSFixture(t)
	fx.setReviews(bot(1, oosFinding))
	edits := func(ctx context.Context) error {
		wt, ok := clonemanager.WorktreeFromContext(ctx)
		if !ok {
			return errors.New("missing leased worktree")
		}
		return clonemanager.WorktreeCallbacks(wt).WriteFile(ctx, "content.txt", "changed\n", 0o644)
	}
	fx.h.reconcile(t, edits, withGiveUp(), fx.opt())
	if fx.stored() {
		t.Errorf("out_of_scope record stored for evidence the run changed (PR body %q)", fx.h.prText)
	}
}

func TestOutOfScopeDecisionRequiresCheckedOutHead(t *testing.T) {
	fx := newOOSFixture(t)
	fx.h.liveHead = strings.Repeat("a", 40)
	fx.first(oosFinding)
	if fx.stored() {
		t.Errorf("stored decision for a different checkout: got = %t, want = false", fx.stored())
	}
}

func TestOutOfScopeDecisionIsNotAppliedOnRebase(t *testing.T) {
	fx := newOOSFixture(t)
	fx.first(oosFinding)
	if stored := fx.stored(); !stored {
		t.Fatalf("stored decision before the conflict: got = %t, want = true", stored)
	}
	fx.h.setPR("CONFLICTING", "[]")
	fx.setReviews(bot(2, oosFinding))
	reads := fx.blobReads
	fx.h.reconcile(t, noChange, withGiveUp(), fx.opt())
	fx.h.wantRun(t, "conflicting PR", 2)
	if fx.blobReads != reads {
		t.Errorf("evidence reads on the default branch: got = %d, want = %d", fx.blobReads, reads)
	}
}

func TestOutOfScopeSuppressionRequeuesWhenCheckoutChanges(t *testing.T) {
	fx := newOOSFixture(t)
	fx.first(oosFinding)
	if stored := fx.stored(); !stored {
		t.Fatalf("stored decision before the checkout changed: got = %t, want = true", stored)
	}
	fx.h.liveHead = strings.Repeat("a", 40)
	fx.setReviews(bot(2, oosFinding), bot(3, "A new independent issue."))
	err := fx.h.reconcileErr(t, t.Context(), noChange, withGiveUp(), fx.opt())
	if _, ok := workqueue.GetRequeueDelay(err); !ok {
		t.Fatalf("reconcile: got = %v, want = requeue to read the new head", err)
	}
	fx.h.wantRun(t, "checkout moved", 1)
}
