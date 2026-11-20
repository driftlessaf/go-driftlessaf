/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metareconciler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-github/v88/github"
)

const (
	testCheckIdentity = "test-bot-evidence"
	testCheckApp      = int64(1234567)
	otherCheckApp     = int64(42)
)

var (
	checkRunsPath   = regexp.MustCompile(`^/repos/owner/repo/commits/([0-9a-f]+)/check-runs$`)
	checkRunPatchID = regexp.MustCompile(`^/repos/owner/repo/check-runs/(\d+)$`)
)

// checkFake serves the check run endpoints of a fake GitHub, keeping one run
// per commit and name and counting the writes that reach it.
type checkFake struct {
	next   int64
	appID  int64 // the App the fake authenticates writes as
	runs   []*github.CheckRun
	writes int

	// failGet, failPost and failPatch, when set, make the next such request
	// answer with that status once and change nothing.
	failGet, failPost, failPatch int

	// skipGets is the number of check-runs reads served before failGet applies.
	skipGets int
}

// takeFailure returns and clears a one-shot failure status.
func takeFailure(status *int) int {
	s := *status
	*status = 0
	return s
}

// takeSkip reports whether no reads remain to let through, spending one if so.
func (c *checkFake) takeSkip() bool {
	if c.skipGets > 0 {
		c.skipGets--
		return false
	}
	return true
}

func (c *checkFake) serve(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && checkRunsPath.MatchString(r.URL.Path) && c.failGet != 0 && c.takeSkip():
		http.Error(w, "injected failure", takeFailure(&c.failGet))
	case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/check-runs" && c.failPost != 0:
		http.Error(w, "injected failure", takeFailure(&c.failPost))
	case r.Method == http.MethodPatch && checkRunPatchID.MatchString(r.URL.Path) && c.failPatch != 0:
		http.Error(w, "injected failure", takeFailure(&c.failPatch))
	case r.Method == http.MethodGet && checkRunsPath.MatchString(r.URL.Path):
		sha := checkRunsPath.FindStringSubmatch(r.URL.Path)[1]
		found := []*github.CheckRun{}
		for _, run := range c.runs {
			if run.GetHeadSHA() == sha {
				found = append(found, run)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(found), "check_runs": found})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/check-runs":
		var opts github.CreateCheckRunOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		c.next++
		c.writes++
		run := &github.CheckRun{
			ID:         new(c.next),
			Name:       new(opts.Name),
			HeadSHA:    new(opts.HeadSHA),
			Status:     opts.Status,
			Conclusion: opts.Conclusion,
			Output:     opts.Output,
			App:        &github.App{ID: new(c.appID)},
		}
		c.runs = append(c.runs, run)
		_ = json.NewEncoder(w).Encode(run)
	case r.Method == http.MethodPatch && checkRunPatchID.MatchString(r.URL.Path):
		var opts github.UpdateCheckRunOptions
		_ = json.NewDecoder(r.Body).Decode(&opts)
		c.writes++
		for _, run := range c.runs {
			if r.URL.Path == "/repos/owner/repo/check-runs/"+strconv.FormatInt(run.GetID(), 10) {
				run.Status, run.Conclusion, run.Output = opts.Status, opts.Conclusion, opts.Output
				_ = json.NewEncoder(w).Encode(run)
				return true
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		return false
	}
	return true
}

// checkHarness opens a PR for an issue with body, built by a real run under
// the evidence check, and returns the harness with the PR's branch at its
// pushed tip and the commit that run pushed.
func checkHarness(t *testing.T, body string, checks string) (*issueHarness, *steeringFake, *checkFake, string) {
	t.Helper()
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	h := newIssueHarness(t)
	f := &steeringFake{h: h}
	c := &checkFake{appID: testCheckApp}
	h.extra = func(w http.ResponseWriter, r *http.Request) bool { return c.serve(w, r) || f.serve(w, r) }
	h.setIssueBody(body)
	h.verdict, h.summary = "changed", "created it"
	h.reconcile(t, writes("one"), withSteering(), withEvidenceCheck())
	pushed := h.branchHead(t)
	h.openPR(t, "MERGEABLE", checks)
	// openPR points the branch at main; keep it at the pushed tip.
	if err := h.remote.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("test-bot/issue-123"), plumbing.NewHash(pushed))); err != nil {
		t.Fatalf("restore PR branch to pushed tip: %v", err)
	}
	h.mu.Lock()
	h.prText = h.prBodies[0]
	h.prLabels = []string{requiredLabel}
	h.mu.Unlock()
	return h, f, c, pushed
}

func withEvidenceCheck() Option[*testRequest, *publishResult, struct{}] {
	return WithEvidenceCheck[*testRequest, *publishResult, struct{}](testCheckIdentity, testCheckApp)
}

// readCheck is what a consumer sees for head, issue body and steering ID.
func readCheck(t *testing.T, h *issueHarness, head, body, steeringID string) *EvidenceCheckDetails {
	t.Helper()
	d, err := ReadEvidenceCheck(t.Context(), h.client(t), "owner", "repo", testCheckIdentity, testCheckApp, 123, head, sha256.Sum256([]byte(body)), steeringID)
	if err != nil {
		t.Fatalf("ReadEvidenceCheck(): %v", err)
	}
	return d
}

func TestEvidenceCheckInitialPR(t *testing.T) {
	body := steeringBody()
	h, _, c, pushed := checkHarness(t, body, "[]")
	d := readCheck(t, h, pushed, body, "")
	if d == nil || d.Verdict != EvidenceVerdictChanged || d.HeadSHA != pushed || d.InputHeadSHA != "" {
		t.Fatalf("ReadEvidenceCheck(): got = %+v, want changed on the pushed head %q", d, pushed)
	}
	if c.writes != 1 {
		t.Errorf("check writes: got = %d, want = 1", c.writes)
	}
	if got := *c.runs[0].Conclusion; got != "neutral" {
		t.Errorf("check conclusion: got = %q, want = neutral", got)
	}
}

func TestEvidenceCheckPush(t *testing.T) {
	body := steeringBody()
	h, _, _, inputHead := checkHarness(t, body, failingCheck)

	body = steeringBody("b")
	h.setIssueBody(body)
	h.verdict, h.summary = "changed", "addressed the note"
	h.reconcile(t, writes("two"), withSteering(), withEvidenceCheck())
	pushed := h.branchHead(t)
	if pushed == inputHead {
		t.Fatal("the run did not push")
	}

	d := readCheck(t, h, pushed, body, "b")
	if d == nil || d.Verdict != EvidenceVerdictChanged || d.InputHeadSHA != inputHead {
		t.Fatalf("ReadEvidenceCheck(): got = %+v, want changed from %q", d, inputHead)
	}
	if len(d.Checks) != 1 || d.Checks[0].HeadSHA != inputHead {
		t.Errorf("checks: got = %+v, want one on the input head %q", d.Checks, inputHead)
	}
	// The check seen on the input head is never credited to the pushed head,
	// and the push says nothing about the input head under the new intent.
	if got := (&Evidence{HeadSHA: d.HeadSHA, Checks: d.Checks}).ChecksForHead(); len(got) != 0 {
		t.Errorf("checks credited to the pushed head: got = %+v, want none", got)
	}
	if d := readCheck(t, h, inputHead, body, "b"); d != nil {
		t.Errorf("ReadEvidenceCheck() on the input head: got = %+v, want unknown", d)
	}
	if d := readCheck(t, h, pushed, steeringBody("c"), "c"); d != nil {
		t.Errorf("ReadEvidenceCheck() under another intent: got = %+v, want unknown", d)
	}
}

// A no-op on the head a push left replaces the push's "changed", and repeating
// it, however the agent words it, writes nothing and never edits the PR body.
func TestEvidenceCheckChangedThenNoOp(t *testing.T) {
	body := steeringBody()
	h, f, c, pushed := checkHarness(t, body, failingCheck)
	if d := readCheck(t, h, pushed, body, ""); d == nil || d.Verdict != EvidenceVerdictChanged {
		t.Fatalf("ReadEvidenceCheck() before: got = %+v, want changed", d)
	}

	h.verdict, h.summary = "no_change", "first"
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	d := readCheck(t, h, pushed, body, "")
	if d == nil || d.Verdict != EvidenceVerdictNoChange || d.HeadSHA != pushed {
		t.Fatalf("ReadEvidenceCheck() after the no-op: got = %+v, want no_change on %q", d, pushed)
	}
	if got := d.Checks; len(got) != 1 || got[0].HeadSHA != pushed {
		t.Errorf("checks: got = %+v, want the one seen on %q", got, pushed)
	}
	writes := c.writes

	h.summary = "second, worded differently"
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if c.writes != writes {
		t.Errorf("check writes by a repeated no-op: got = %d, want = %d", c.writes, writes)
	}
	f.wantWrites(t, "no-ops", 0)
	if len(c.runs) != 1 {
		t.Errorf("check runs: got = %d, want = 1", len(c.runs))
	}
}

// The check a no-op writes queues the issue again. With the same head, intent
// and findings the agent must not run for it, whatever the outcome would be.
func TestEvidenceCheckOwnEventDoesNotRerunAgent(t *testing.T) {
	body := steeringBody()
	h, _, c, pushed := checkHarness(t, body, failingCheck)

	h.verdict, h.summary = "no_change", "first"
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	calls, writes := h.agentCalls, c.writes
	if d := readCheck(t, h, pushed, body, ""); d == nil || d.Verdict != EvidenceVerdictNoChange {
		t.Fatalf("ReadEvidenceCheck(): got = %+v, want no_change", d)
	}

	// The event of that write, with an agent that would now say otherwise.
	h.verdict, h.summary = "", ""
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if h.agentCalls != calls {
		t.Errorf("agent calls after the check event: got = %d, want = %d", h.agentCalls, calls)
	}
	if c.writes != writes {
		t.Errorf("check writes after the check event: got = %d, want = %d", c.writes, writes)
	}
}

// A same-head attempt that cannot stand behind a verdict supersedes the
// earlier "changed", and repeating it writes nothing.
func TestEvidenceCheckChangedThenUnknown(t *testing.T) {
	body := steeringBody()
	h, f, c, pushed := checkHarness(t, body, failingCheck)

	// A human edits the description while the agent runs.
	edit := func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prText += "\nedited by a person"
		return nil
	}
	h.verdict, h.summary = "", ""
	if err := h.reconcileErr(t, t.Context(), edit, withSteering(), withEvidenceCheck()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d := readCheck(t, h, pushed, body, ""); d != nil {
		t.Errorf("ReadEvidenceCheck() after an unknown attempt: got = %+v, want unknown", d)
	}
	if len(c.runs) != 1 || !strings.Contains(c.runs[0].GetOutput().GetTitle(), EvidenceVerdictUnknown) {
		t.Errorf("check runs: got = %+v, want one recording unknown", c.runs)
	}
	writes := c.writes

	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if c.writes != writes {
		t.Errorf("check writes by a repeated unknown: got = %d, want = %d", c.writes, writes)
	}
	f.wantWrites(t, "unknown attempts", 0)
	h.mu.Lock()
	kept := strings.HasSuffix(h.prText, "edited by a person")
	h.mu.Unlock()
	if !kept {
		t.Error("PR body: got = edit lost, want the person's edit kept")
	}
}

// With the check on, a changed issue alone never rewrites the PR description.
func TestEvidenceCheckIssueBodyEditKeepsProse(t *testing.T) {
	h, f, _, pushed := checkHarness(t, steeringBody(), failingCheck)
	body := steeringBody() + "\nA clarification."
	h.setIssueBody(body)
	h.verdict, h.summary = "no_change", "nothing to do"
	edit := func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prText += "\nedited by a person"
		return nil
	}
	if err := h.reconcileErr(t, t.Context(), edit, withSteering(), withEvidenceCheck()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	h.mu.Lock()
	kept := strings.Contains(h.prText, "edited by a person")
	h.mu.Unlock()
	if !kept {
		t.Error("PR body: got = edit lost, want the person's edit kept")
	}
	f.wantWrites(t, "issue body edit", 0)
	if d := readCheck(t, h, pushed, body, ""); d == nil || d.Verdict != EvidenceVerdictNoChange {
		t.Errorf("ReadEvidenceCheck(): got = %+v, want no_change under the new intent", d)
	}
}

// A run on a commit other than the PR head the session read makes no claim
// about either.
func TestEvidenceCheckCheckoutMismatchIsUnknown(t *testing.T) {
	body := steeringBody()
	h, _, c, pushed := checkHarness(t, body, failingCheck)
	h.mu.Lock()
	h.liveHead = sha("c")
	h.mu.Unlock()
	h.verdict, h.summary = "no_change", "nothing to do"
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if d := readCheck(t, h, sha("c"), body, ""); d != nil {
		t.Errorf("ReadEvidenceCheck() on the live head: got = %+v, want unknown", d)
	}
	if d := readCheck(t, h, pushed, body, ""); d != nil {
		t.Errorf("ReadEvidenceCheck() on the checked-out head: got = %+v, want unknown", d)
	}
	// The attempt evaluated the checked-out commit. Its earlier changed
	// record must be superseded even though the API returned another head.
	found := false
	for _, r := range c.runs {
		if r.GetHeadSHA() == sha("c") {
			t.Errorf("published on an unevaluated commit: %s", r.GetHeadSHA())
		}
		if r.GetHeadSHA() == pushed && strings.Contains(r.GetOutput().GetTitle(), EvidenceVerdictUnknown) {
			found = true
		}
	}
	if !found {
		t.Errorf("check runs: got = %+v, want unknown on %s", c.runs, pushed)
	}
}

// A failed write over an existing record is returned so the queue retries and
// the stale verdict does not stay the latest attempt. A failed first create is dropped.
func TestEvidenceCheckWriteFailures(t *testing.T) {
	tests := []struct {
		name     string
		inject   func(c *checkFake)
		existing bool
		wantErr  bool
		// skipGets lets that many check-runs reads through before the
		// injected failure: the guard's read comes first, then publish's.
		skipGets int
		errPart  string
	}{
		{name: "patch 500 over a record", inject: func(c *checkFake) { c.failPatch = http.StatusInternalServerError }, existing: true, wantErr: true},
		{name: "get 500 over a record", inject: func(c *checkFake) { c.failGet = http.StatusInternalServerError }, existing: true, wantErr: true, skipGets: 1, errPart: "read evidence check"},
		{name: "patch 403 over a record", inject: func(c *checkFake) { c.failPatch = http.StatusForbidden }, existing: true, wantErr: true},
		{name: "first create 500", inject: func(c *checkFake) { c.failPost = http.StatusInternalServerError }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := steeringBody()
			h, _, c, pushed := checkHarness(t, body, failingCheck)
			c.skipGets = tt.skipGets
			if !tt.existing {
				c.runs = nil
			}
			oldTitle := ""
			if tt.existing {
				oldTitle = c.runs[0].GetOutput().GetTitle()
			}
			tt.inject(c)
			h.verdict, h.summary = "no_change", "nothing to do"
			err := h.reconcileErr(t, t.Context(), noChange, withSteering(), withEvidenceCheck())
			if (err != nil) != tt.wantErr {
				t.Fatalf("reconcile: got = %v, want error = %v", err, tt.wantErr)
			}
			if tt.errPart != "" && !strings.Contains(err.Error(), tt.errPart) {
				t.Errorf("reconcile error: got = %v, want it to contain %q", err, tt.errPart)
			}
			if tt.existing {
				if got := c.runs[0].GetOutput().GetTitle(); got != oldTitle {
					t.Errorf("stored title: got = %q, want = %q", got, oldTitle)
				}
				if d := readCheck(t, h, pushed, body, ""); d == nil || d.Verdict != EvidenceVerdictChanged {
					t.Errorf("ReadEvidenceCheck(): got = %+v, want the old changed record", d)
				}
			} else if len(c.runs) != 0 {
				t.Errorf("check runs: got = %d, want none", len(c.runs))
			}
		})
	}
}

// An App that cannot read checks must not stop the agent iterating on findings.
func TestEvidenceCheckGuardReadPermissionError(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h, _, c, _ := checkHarness(t, steeringBody(), failingCheck)
			calls := h.agentCalls
			c.failGet = status
			h.verdict, h.summary = "no_change", "nothing to do"
			h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
			if h.agentCalls != calls+1 {
				t.Errorf("agent calls: got = %d, want = %d", h.agentCalls, calls+1)
			}
		})
	}
}

// The guard only holds back the PR-branch findings iteration: a PR that turns
// conflicting at the same head and findings is rebuilt from the default branch.
func TestEvidenceCheckGuardSkipsConflictingRebuild(t *testing.T) {
	h, _, _, _ := checkHarness(t, steeringBody(), failingCheck)
	h.verdict, h.summary = "no_change", "nothing to do"
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	calls := h.agentCalls
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if h.agentCalls != calls {
		t.Fatalf("agent calls on a repeat: got = %d, want = %d", h.agentCalls, calls)
	}
	h.setPR("CONFLICTING", failingCheck)
	h.reconcile(t, noChange, withSteering(), withEvidenceCheck())
	if h.agentCalls != calls+1 {
		t.Errorf("agent calls after the PR conflicted: got = %d, want = %d", h.agentCalls, calls+1)
	}
}

// A check with the same name from another App is never read.
func TestEvidenceCheckIgnoresOtherApp(t *testing.T) {
	body := steeringBody()
	h, _, c, pushed := checkHarness(t, body, "[]")
	c.appID = otherCheckApp
	c.runs = nil
	forger := &Reconciler[*testRequest, *publishResult, struct{}]{evidenceCheck: &evidenceCheckConfig{identity: testCheckIdentity, appID: otherCheckApp}}
	d := newEvidenceCheckDetails(&PRData[*testRequest]{IssueNumber: 123, IssueBodyHash: sha256.Sum256([]byte(body))}, pushed, &Evidence{
		Version: EvidenceVersion, IssueBodyHash: sha256.Sum256([]byte(body)), HeadSHA: pushed, InputHeadSHA: pushed,
		Verdict: EvidenceVerdictNoChange, SummarySource: EvidenceSourceAgentNote,
	})
	forger.publishEvidenceCheck(t.Context(), h.client(t), &githubreconciler.Resource{Owner: "owner", Repo: "repo"}, d)
	if len(c.runs) != 1 {
		t.Fatalf("forged check runs: got = %d, want = 1", len(c.runs))
	}
	if got := readCheck(t, h, pushed, body, ""); got != nil {
		t.Errorf("ReadEvidenceCheck() with a check from another App: got = %+v, want unknown", got)
	}
}

func TestEvidenceCheckIgnoresOtherIssue(t *testing.T) {
	body := steeringBody()
	h, _, _, head := checkHarness(t, body, "[]")
	got, err := ReadEvidenceCheck(t.Context(), h.client(t), "owner", "repo", testCheckIdentity, testCheckApp, 456, head, sha256.Sum256([]byte(body)), "")
	if err != nil || got != nil {
		t.Errorf("read for other issue: got (%+v, %v), want no evidence", got, err)
	}
}

func TestWithEvidenceCheckNeedsApp(t *testing.T) {
	var r Reconciler[*testRequest, *publishResult, struct{}]
	WithEvidenceCheck[*testRequest, *publishResult, struct{}](testCheckIdentity, 0)(&r)
	if r.evidenceCheck != nil {
		t.Error("evidence check without an App ID: got = on, want = off")
	}
	if d, err := ReadEvidenceCheck(t.Context(), nil, "owner", "repo", testCheckIdentity, 0, 123, sha("a"), [32]byte{1}, ""); d != nil || err != nil {
		t.Errorf("ReadEvidenceCheck() without an App ID: got = %v, %v, want none", d, err)
	}
}

func TestEvidenceCheckDetailsValidate(t *testing.T) {
	valid := func() EvidenceCheckDetails {
		return EvidenceCheckDetails{
			Version: EvidenceCheckVersion, IssueNumber: 123, IssueBodyHash: [32]byte{1}, HeadSHA: sha("b"), InputHeadSHA: sha("a"),
			Verdict: EvidenceVerdictChanged,
			Checks:  []ObservedCheck{{Source: EvidenceSourceObservedCheck, Name: "ci", HeadSHA: sha("a"), Conclusion: "failure"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(d *EvidenceCheckDetails)
		wantErr bool
	}{
		{name: "valid", mutate: func(*EvidenceCheckDetails) {}},
		{name: "unknown version", mutate: func(d *EvidenceCheckDetails) { d.Version = 2 }, wantErr: true},
		{name: "no hash", mutate: func(d *EvidenceCheckDetails) { d.IssueBodyHash = [32]byte{} }, wantErr: true},
		{name: "malformed head", mutate: func(d *EvidenceCheckDetails) { d.HeadSHA = "x" }, wantErr: true},
		{name: "check on another head", mutate: func(d *EvidenceCheckDetails) { d.Checks[0].HeadSHA = d.HeadSHA }, wantErr: true},
		{name: "unknown verdict", mutate: func(d *EvidenceCheckDetails) { d.Verdict = "approved" }, wantErr: true},
		{name: "unknown with a claim", mutate: func(d *EvidenceCheckDetails) { d.Verdict = EvidenceVerdictUnknown }, wantErr: true},
		{name: "unknown", mutate: func(d *EvidenceCheckDetails) {
			d.Verdict, d.InputHeadSHA, d.Checks = EvidenceVerdictUnknown, "", nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := valid()
			tt.mutate(&d)
			if err := d.validate(); (err != nil) != tt.wantErr {
				t.Errorf("validate(): got = %v, want error = %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewReviewReplyChangesFindingsDigest(t *testing.T) {
	findings := []callbacks.Finding{{Kind: callbacks.FindingKindReview, Identifier: "thread-1", Details: "Initial finding"}}
	before := findingsDigest(findings)
	findings[0].Details += "\nA teammate added a correction"
	if findingsDigest(findings) == before {
		t.Error("a new reply in the same thread reused the prior attempt")
	}
}
