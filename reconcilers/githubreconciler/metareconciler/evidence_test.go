/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"text/template"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/changemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	"github.com/go-git/go-git/v5/plumbing"
)

// sha returns a well-formed commit SHA made of the hex digit c.
func sha(c string) string { return strings.Repeat(c, 40) }

// decodePRData reads the data embedded in a PR body.
func decodePRData(t *testing.T, body string) *PRData[*testRequest] {
	t.Helper()
	cm, err := changemanager.New[PRData[*testRequest]]("test-bot",
		template.Must(template.New("title").Parse("Update content")),
		template.Must(template.New("body").Parse("Resolve #123.")))
	if err != nil {
		t.Fatalf("create change manager: %v", err)
	}
	data, err := cm.Extract(body)
	if err != nil {
		t.Fatalf("extract PR data: %v", err)
	}
	return data
}

// branchHead is the commit the remote's PR branch points at.
func (h *issueHarness) branchHead(t *testing.T) string {
	t.Helper()
	ref, err := h.remote.Reference(plumbing.NewBranchReferenceName("test-bot/issue-123"), true)
	if err != nil {
		t.Fatalf("PR branch: %v", err)
	}
	return ref.Hash().String()
}

// prData decodes the data the open PR's body records.
func (h *issueHarness) prData(t *testing.T) *PRData[*testRequest] {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	return decodePRData(t, h.prText)
}

// evidenceHarness opens a PR for an issue with body, built by a real run that
// declares verdict, then returns the harness with the declared verdict reset
// to the one the next run declares.
func evidenceHarness(t *testing.T, body, verdict string, checks string) (*issueHarness, *steeringFake) {
	t.Helper()
	h := newIssueHarness(t)
	f := &steeringFake{h: h}
	h.extra = f.serve
	h.setIssueBody(body)
	h.verdict, h.summary = verdict, "created it"
	h.reconcile(t, writes("one"), withSteering())
	if len(h.prBodies) != 1 {
		t.Fatalf("PRs created: got = %d, want = 1", len(h.prBodies))
	}
	h.openPR(t, "MERGEABLE", checks)
	h.mu.Lock()
	h.prText = h.prBodies[0]
	h.prLabels = []string{requiredLabel}
	h.mu.Unlock()
	return h, f
}

func wantDirection(t *testing.T, e *Evidence, body, steeringID string) {
	t.Helper()
	if want := sha256.Sum256([]byte(body)); e.IssueBodyHash != want {
		t.Errorf("evidence issue body hash: got = %x, want = %x", e.IssueBodyHash, want)
	}
	if e.SteeringID != steeringID {
		t.Errorf("evidence steering ID: got = %q, want = %q", e.SteeringID, steeringID)
	}
	if e.SummarySource != EvidenceSourceAgentNote {
		t.Errorf("evidence summary source: got = %q, want = %q", e.SummarySource, EvidenceSourceAgentNote)
	}
}

func TestEvidenceInitialPR(t *testing.T) {
	body := steeringBody("a")
	h := newIssueHarness(t)
	h.setIssueBody(body)
	h.verdict, h.summary = "changed", "created it"
	h.reconcile(t, writes("one"), withSteering())
	if len(h.prBodies) != 1 {
		t.Fatalf("PRs created: got = %d, want = 1", len(h.prBodies))
	}

	e, err := decodePRData(t, h.prBodies[0]).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if want := h.branchHead(t); e.HeadSHA != want {
		t.Errorf("evidence head: got = %q, want = %q", e.HeadSHA, want)
	}
	if e.InputHeadSHA != "" {
		t.Errorf("evidence input head: got = %q, want none", e.InputHeadSHA)
	}
	if e.Verdict != "changed" || e.Summary != "created it" {
		t.Errorf("evidence verdict and summary: got = %q, %q, want = %q, %q", e.Verdict, e.Summary, "changed", "created it")
	}
	wantDirection(t, e, body, "a")
}

func TestEvidencePush(t *testing.T) {
	body := steeringBody()
	h, _ := evidenceHarness(t, body, "changed", failingCheck)
	first, err := h.prData(t).EvidenceRecord()
	if err != nil || first == nil {
		t.Fatalf("EvidenceRecord() at creation: got = %v, %v, want evidence", first, err)
	}

	// The PR head the run starts from, which the harness's PR query reports.
	inputHead := h.branchHead(t)

	body = steeringBody("b")
	h.setIssueBody(body)
	h.verdict, h.summary = "changed", "addressed the note"
	h.reconcile(t, writes("two"), withSteering())

	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	pushed := h.branchHead(t)
	if e.HeadSHA != pushed {
		t.Errorf("evidence head: got = %q, want the pushed %q", e.HeadSHA, pushed)
	}
	if e.HeadSHA == first.HeadSHA || e.HeadSHA == inputHead {
		t.Errorf("evidence head %q: want a head different from the input head %q and the earlier %q", e.HeadSHA, inputHead, first.HeadSHA)
	}
	if e.InputHeadSHA != inputHead {
		t.Errorf("evidence input head: got = %q, want = %q", e.InputHeadSHA, inputHead)
	}
	wantDirection(t, e, body, "b")

	// The failing check ran on the input head. It is recorded against that
	// head, and never credited to the pushed one.
	if len(e.Checks) != 1 || e.Checks[0].HeadSHA != inputHead || e.Checks[0].Source != EvidenceSourceObservedCheck {
		t.Errorf("evidence checks: got = %+v, want one observed check on %q", e.Checks, inputHead)
	}
	if got := e.ChecksForHead(); len(got) != 0 {
		t.Errorf("checks credited to the pushed head: got = %+v, want none", got)
	}
}

// The session read one head, but the branch tip the agent checked out is
// another: a check read for the session's head is not recorded for the tip.
func TestEvidencePushCheckoutMismatch(t *testing.T) {
	h, _ := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	tip := h.branchHead(t)
	h.mu.Lock()
	h.liveHead = sha("c")
	h.mu.Unlock()

	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "changed", "addressed the note"
	h.reconcile(t, writes("two"), withSteering())

	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if len(e.Checks) != 0 {
		t.Errorf("evidence checks: got = %+v, want none", e.Checks)
	}
	if e.InputHeadSHA != tip {
		t.Errorf("evidence input head: got = %q, want the real tip %q", e.InputHeadSHA, tip)
	}
}

// A push from the default branch checkout never saw the PR head, so no input
// head or checks are recorded.
func TestEvidencePushDefaultBranch(t *testing.T) {
	h, _ := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.setPR("CONFLICTING", failingCheck)

	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "changed", "addressed the note"
	h.reconcile(t, writes("two"), withSteering())

	e, err := h.prData(t).EvidenceRecord()
	if err != nil {
		t.Fatalf("EvidenceRecord(): %v", err)
	}
	if e == nil {
		t.Fatal("EvidenceRecord(): got = nil, want evidence from the default-branch push")
	}
	if e.SteeringID != "b" {
		t.Errorf("evidence steering ID: got = %q, want %q", e.SteeringID, "b")
	}
	if want := h.branchHead(t); e.HeadSHA != want {
		t.Errorf("evidence head: got = %q, want the pushed head %q", e.HeadSHA, want)
	}
	if e.InputHeadSHA != "" || len(e.Checks) != 0 {
		t.Errorf("evidence from a default-branch push: got = %+v, want no input head or checks", e)
	}
}

func TestEvidenceNoOp(t *testing.T) {
	body := steeringBody()
	h, f := evidenceHarness(t, body, "changed", failingCheck)

	// The checkout is the PR branch's tip, which is the head the PR reports.
	inputHead := h.branchHead(t)

	body = steeringBody("b")
	h.setIssueBody(body)
	h.verdict, h.summary = "no_change", "nothing to do"
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "no-op", 1)

	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if e.HeadSHA != inputHead || e.InputHeadSHA != inputHead {
		t.Errorf("evidence heads: got = %q and %q, want both = %q", e.HeadSHA, e.InputHeadSHA, inputHead)
	}
	if e.Verdict != "no_change" || e.Summary != "nothing to do" {
		t.Errorf("evidence verdict and summary: got = %q, %q", e.Verdict, e.Summary)
	}
	wantDirection(t, e, body, "b")

	// Nothing was pushed, so the check on the input head is on the head the
	// agent evaluated.
	if got := e.ChecksForHead(); len(got) != 1 || got[0].Name != "unit-tests" {
		t.Errorf("checks for the evaluated head: got = %+v, want unit-tests", got)
	}
}

// A no-op run that changes nothing about the bound head or direction must not
// edit the PR body again, or the edit event would start another run.
func TestEvidenceNoOpRepeatDoesNotRewrite(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.mu.Lock()
	before := h.prText
	h.mu.Unlock()
	h.verdict, h.summary = "no_change", "first"
	h.reconcile(t, noChange, withSteering())
	h.verdict, h.summary = "no_change", "second"
	h.reconcile(t, noChange, withSteering())
	h.wantRun(t, "repeated no-op", 3)
	f.wantWrites(t, "repeated no-op", 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prText != before {
		t.Error("PR body after the no-ops: got = changed, want it left untouched")
	}
}

// A same-head no-op right after a run that changed the PR writes nothing to
// the editable PR body: the stored record keeps describing the pushed head.
func TestEvidenceNoOpAfterChangedWritesNothing(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	before, err := h.prData(t).EvidenceRecord()
	if err != nil || before == nil || before.Verdict != EvidenceVerdictChanged {
		t.Fatalf("EvidenceRecord() before: got = %+v, %v, want a changed record", before, err)
	}
	// openPR reset the branch to main; keep it at the pushed tip the changed
	// record describes, so only the verdict differs on the no-op.
	if err := h.remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("test-bot/issue-123"), plumbing.NewHash(before.HeadSHA))); err != nil {
		t.Fatalf("restore PR branch to pushed tip: %v", err)
	}
	h.verdict, h.summary = "no_change", "first"
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "no-op after changed", 0)
	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if e.HeadSHA != before.HeadSHA || e.Verdict != EvidenceVerdictChanged {
		t.Errorf("evidence: got = %q %q, want the pushed head %q recorded as changed", e.HeadSHA, e.Verdict, before.HeadSHA)
	}
	h.summary = "second"
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "repeated no-op", 0)
}

// A no-op that can no longer stand behind the stored record (it declares no
// verdict) never rewrites the PR description just to clear it, so a
// concurrent human edit survives and repeats write nothing.
func TestEvidenceNoOpInvalidationDoesNotWriteBody(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.verdict, h.summary = "", ""
	edit := func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prText += "\nedited by a person"
		return nil
	}
	if err := h.reconcileErr(t, t.Context(), edit, withSteering()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.wantWrites(t, "invalidating no-op", 0)
	h.mu.Lock()
	kept := strings.HasSuffix(h.prText, "edited by a person")
	h.mu.Unlock()
	if !kept {
		t.Error("PR body: got = edit lost, want the person's edit kept")
	}
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "repeated invalidating no-op", 0)
}

// A person editing the PR body during a plain no-op is never overwritten:
// nothing is written to it.
func TestEvidenceNoOpConcurrentBodyEditSurvives(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.verdict, h.summary = "no_change", "nothing to do"
	edit := func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prText += "\nedited by a person"
		return nil
	}
	if err := h.reconcileErr(t, t.Context(), edit, withSteering()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.wantWrites(t, "no-op with a concurrent edit", 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.HasSuffix(h.prText, "edited by a person") {
		t.Error("PR body: got = edit lost, want the person's edit kept")
	}
}

// A PR-body record remains tied to the issue text the push consumed.
func TestEvidenceNoOpIssueBodyHashChangeLeavesPushEvidenceHistorical(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.verdict, h.summary = "no_change", "first"
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "first no-op", 0)

	body := steeringBody() + "\nA clarification with no steering note."
	h.setIssueBody(body)
	h.verdict, h.summary = "no_change", "second"
	h.reconcile(t, noChange, withSteering())
	f.wantWrites(t, "no-op after the issue body changed", 0)

	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if want := sha256.Sum256([]byte(steeringBody())); e.IssueBodyHash != want {
		t.Errorf("historical evidence issue body hash: got = %x, want = %x", e.IssueBodyHash, want)
	}
}

func TestEvidencePushWithContradictoryDeclarationIsUnknown(t *testing.T) {
	h, _ := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "no_change", "already fixed"
	h.reconcile(t, writes("two"), withSteering())

	if got, err := h.prData(t).EvidenceRecord(); got != nil || err != nil {
		t.Errorf("EvidenceRecord after contradictory declaration: got = %v, %v, want nil, nil", got, err)
	}
}

// noOpEvidence runs a no-op reconcile that consumes a new steering note and
// returns the evidence the PR then records, failing if it cannot be read.
func noOpEvidence(t *testing.T, h *issueHarness, agent func(context.Context) error) *Evidence {
	t.Helper()
	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "no_change", "nothing to do"
	h.reconcile(t, agent, withSteering())
	e, err := h.prData(t).EvidenceRecord()
	if err != nil {
		t.Fatalf("EvidenceRecord(): %v", err)
	}
	return e
}

// The PR reports a head the agent did not check out: a push moved the branch
// between the session's read and the lease. The run makes no claim about it.
func TestEvidenceNoOpCheckoutMismatch(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	h.mu.Lock()
	h.liveHead = sha("c")
	h.mu.Unlock()
	if e := noOpEvidence(t, h, noChange); e != nil {
		t.Errorf("evidence on a head the agent did not see: got = %+v, want none", e)
	}
	f.wantWrites(t, "mismatched no-op", 1)
	if got := h.recorded(); got != "b" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "b")
	}
}

// A run that starts from the default branch never saw the PR head, even when
// the default branch's tip is that head.
func TestEvidenceNoOpDefaultBranch(t *testing.T) {
	h, _ := evidenceHarness(t, steeringBody(), "changed", "[]")
	h.setPR("CONFLICTING", "[]")
	if e := noOpEvidence(t, h, noChange); e != nil {
		t.Errorf("evidence from a default-branch run: got = %+v, want none", e)
	}
}

// The commit guard drops every edit, so nothing landed: the run does not
// record the agent's "changed" verdict.
func TestEvidenceNoOpGuardDroppedEdits(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	h, _ := evidenceHarness(t, steeringBody(), "changed", failingCheck)
	dl := commitscope.DefaultDenylist()
	dl.DenyWorkflowEdits = true
	h.cloneOpts = []clonemanager.Option{clonemanager.WithCommitScopeGuard(dl)}

	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "changed", "edited a workflow"
	h.reconcile(t, writesWorkflow, withSteering())
	e, err := h.prData(t).EvidenceRecord()
	if err != nil || e != nil {
		t.Errorf("EvidenceRecord() after dropped edits: got = %+v, %v, want none", e, err)
	}
}

// The PR moved on, or a person edited it, while the agent ran: the re-embed
// must not overwrite it with a record of the earlier attempt.
func TestEvidenceNoOpRefusesStaleReembed(t *testing.T) {
	tests := []struct {
		name  string
		agent func(h *issueHarness) func(context.Context) error
	}{{
		name: "the head advances",
		agent: func(h *issueHarness) func(context.Context) error {
			return func(context.Context) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.liveHead = sha("d")
				return nil
			}
		},
	}, {
		name: "the body is edited",
		agent: func(h *issueHarness) func(context.Context) error {
			return func(context.Context) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.prText += "\nedited by a person"
				return nil
			}
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := evidenceHarness(t, steeringBody(), "changed", failingCheck)
			h.setIssueBody(steeringBody("b"))
			h.verdict, h.summary = "no_change", "nothing to do"
			if err := h.reconcileErr(t, t.Context(), tt.agent(h), withSteering()); err == nil {
				t.Error("reconcile: got = nil, want an error refusing the stale re-embed")
			}
			f.wantWrites(t, "stale re-embed", 0)
		})
	}
}

// An open, ready PR whose body carries an invalid evidence record, with
// nothing else changed, must not run the agent or be written again.
func TestEvidenceInvalidStoredRecordDoesNotRerun(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", "[]")
	h.mu.Lock()
	bad := strings.Replace(h.prText, fmt.Sprintf(`"version": %d`, EvidenceVersion), fmt.Sprintf(`"version": %d`, EvidenceVersion+1), 1)
	if bad == h.prText {
		h.mu.Unlock()
		t.Fatal("could not corrupt the stored evidence version")
	}
	h.prText = bad
	h.prLabels = []string{requiredLabel, readyLabel}
	h.agentCalls = 0
	h.mu.Unlock()
	if e, err := h.prData(t).EvidenceRecord(); e != nil || err == nil {
		t.Fatalf("EvidenceRecord() of the corrupted record: got = %v, %v, want an error", e, err)
	}

	h.reconcile(t, writes("two"), withSteering())
	h.wantRun(t, "invalid stored record", 0)
	f.wantWrites(t, "invalid stored record", 0)
}

// A steer on a run that cannot claim a head (here, a default-branch run)
// records the steering ID with the long-standing data-block-only write, which
// keeps a person's prose edit made mid-run.
func TestEvidenceNoOpNoEvidenceKeepsEditedProse(t *testing.T) {
	h, f := evidenceHarness(t, steeringBody(), "changed", "[]")
	h.setPR("CONFLICTING", "[]")
	h.setIssueBody(steeringBody("b"))
	h.verdict, h.summary = "no_change", "nothing to do"
	edit := func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prText += "\nedited by a person"
		return nil
	}
	if err := h.reconcileErr(t, t.Context(), edit, withSteering()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.wantWrites(t, "edited prose without evidence", 1)
	h.mu.Lock()
	kept := strings.Contains(h.prText, "edited by a person")
	h.mu.Unlock()
	if !kept {
		t.Error("PR body: got = edit lost, want the person's edit kept")
	}
	if got := h.recorded(); got != "b" {
		t.Errorf("recorded steering ID: got = %q, want = %q", got, "b")
	}
}

func TestEvidenceNotDeclared(t *testing.T) {
	h, _ := evidenceHarness(t, steeringBody(), "", "[]")
	if e, err := h.prData(t).EvidenceRecord(); e != nil || err != nil {
		t.Errorf("EvidenceRecord() with no declared verdict: got = %v, %v, want none", e, err)
	}
}

func TestEvidenceSummaryBounded(t *testing.T) {
	h := newIssueHarness(t)
	h.verdict, h.summary = "changed", strings.Repeat("é", 2*evidenceSummaryMaxRunes)
	h.reconcile(t, writes("one"), withSteering())
	e, err := decodePRData(t, h.prBodies[0]).EvidenceRecord()
	if err != nil || e == nil {
		t.Fatalf("EvidenceRecord(): got = %v, %v, want evidence", e, err)
	}
	if got := len([]rune(e.Summary)); got != evidenceSummaryMaxRunes {
		t.Errorf("summary length: got = %d, want = %d", got, evidenceSummaryMaxRunes)
	}
}

func TestEvidenceRecordReadsOldAndRejectsUnknown(t *testing.T) {
	hash := sha256.Sum256([]byte("body"))
	valid := func() *Evidence {
		return &Evidence{
			Version:       EvidenceVersion,
			IssueBodyHash: hash,
			SteeringID:    "a",
			HeadSHA:       sha("b"),
			InputHeadSHA:  sha("a"),
			Verdict:       "changed",
			SummarySource: EvidenceSourceAgentNote,
			Checks:        []ObservedCheck{{Source: EvidenceSourceObservedCheck, Name: "ci", HeadSHA: sha("a"), Conclusion: "failure"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(e *Evidence)
		wantErr bool
	}{
		{name: "valid", mutate: func(*Evidence) {}},
		{name: "unknown version", mutate: func(e *Evidence) { e.Version = EvidenceVersion + 1 }, wantErr: true},
		{name: "no version", mutate: func(e *Evidence) { e.Version = 0 }, wantErr: true},
		{name: "no head", mutate: func(e *Evidence) { e.HeadSHA = "" }, wantErr: true},
		{name: "no verdict", mutate: func(e *Evidence) { e.Verdict = "" }, wantErr: true},
		{name: "unknown verdict", mutate: func(e *Evidence) { e.Verdict = "approved" }, wantErr: true},
		{name: "unknown summary source", mutate: func(e *Evidence) { e.SummarySource = "check" }, wantErr: true},
		{name: "check without a head", mutate: func(e *Evidence) { e.Checks[0].HeadSHA = "" }, wantErr: true},
		{name: "check from an unknown source", mutate: func(e *Evidence) { e.Checks[0].Source = EvidenceSourceAgentNote }, wantErr: true},
		{name: "check on another head than the input head", mutate: func(e *Evidence) { e.Checks[0].HeadSHA = e.HeadSHA }, wantErr: true},
		{name: "check without an input head", mutate: func(e *Evidence) { e.InputHeadSHA = "" }, wantErr: true},
		{name: "check without a conclusion", mutate: func(e *Evidence) { e.Checks[0].Conclusion = "" }, wantErr: true},
		{name: "check without a name", mutate: func(e *Evidence) { e.Checks[0].Name = "" }, wantErr: true},
		{name: "zero issue body hash", mutate: func(e *Evidence) { e.IssueBodyHash = [32]byte{} }, wantErr: true},
		{name: "malformed head", mutate: func(e *Evidence) { e.HeadSHA = "head" }, wantErr: true},
		{name: "malformed input head", mutate: func(e *Evidence) { e.InputHeadSHA = "abc123" }, wantErr: true},
		{name: "too many checks", mutate: func(e *Evidence) {
			e.Checks = slices.Repeat(e.Checks, evidenceMaxChecks+1)
		}, wantErr: true},
		{name: "a change claimed on the input head", mutate: func(e *Evidence) {
			e.HeadSHA, e.Checks = e.InputHeadSHA, nil
		}, wantErr: true},
		{name: "an unchanged head with no change claimed", mutate: func(e *Evidence) {
			e.HeadSHA, e.Verdict, e.Checks = e.InputHeadSHA, "no_change", nil
		}},
		{name: "another issue body", mutate: func(e *Evidence) { e.IssueBodyHash = sha256.Sum256([]byte("other")) }, wantErr: true},
		{name: "another steering ID", mutate: func(e *Evidence) { e.SteeringID = "b" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := valid()
			tt.mutate(e)
			data := &PRData[*testRequest]{IssueBodyHash: hash, SteeringID: "a", Evidence: e}
			got, err := data.EvidenceRecord()
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvidenceRecord() error: got = %v, want error = %v", err, tt.wantErr)
			}
			if tt.wantErr && got != nil {
				t.Errorf("EvidenceRecord(): got = %+v, want none on error", got)
			}
		})
	}

	// Data written before evidence existed decodes, with the evidence unknown.
	var old PRData[*testRequest]
	if err := json.Unmarshal([]byte(`{"identity":"test-bot","issue_url":"u","issue_number":1,"issue_body_hash":[0]}`), &old); err != nil {
		t.Fatalf("decode old data: %v", err)
	}
	if e, err := old.EvidenceRecord(); e != nil || err != nil {
		t.Errorf("EvidenceRecord() on old data: got = %v, %v, want unknown", e, err)
	}
}

func TestNewEvidenceBoundsChecks(t *testing.T) {
	var findings []callbacks.Finding
	for i := range evidenceMaxChecks + 5 {
		name := fmt.Sprintf("check-%d", i)
		if i == 0 {
			name = strings.Repeat("n", evidenceCheckNameMaxRunes+10)
		}
		findings = append(findings, callbacks.Finding{Kind: callbacks.FindingKindCICheck, Name: name})
	}
	data := &PRData[*testRequest]{IssueBodyHash: sha256.Sum256([]byte("body"))}
	result := &publishResult{verdict: "changed", summary: "s"}

	e := newEvidence(data, result, sha("b"), sha("a"), findings)
	if e == nil {
		t.Fatal("newEvidence(): got = nil, want evidence")
	}
	if got, want := len(e.Checks), evidenceMaxChecks; got != want {
		t.Errorf("checks: got = %d, want = %d", got, want)
	}
	if got, want := len([]rune(e.Checks[0].Name)), evidenceCheckNameMaxRunes; got != want {
		t.Errorf("long check name length: got = %d, want = %d", got, want)
	}
	for _, c := range e.Checks {
		if c.HeadSHA != sha("a") {
			t.Errorf("check head: got = %q, want input head", c.HeadSHA)
		}
	}
}
