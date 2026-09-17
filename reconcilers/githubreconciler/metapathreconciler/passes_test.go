/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-cmp/cmp"
)

// fakeResp is a minimal GetCommitMessage() carrier for combineCommitMessages.
type fakeResp struct {
	msg string
}

func (f fakeResp) GetCommitMessage() string { return f.msg }

func TestCombineCommitMessages(t *testing.T) {
	tests := []struct {
		name    string
		groups  []FanOutGroup
		results []fakeResp
		want    string
	}{{
		name:    "single result kept verbatim",
		groups:  []FanOutGroup{{Key: "skill/one"}},
		results: []fakeResp{{msg: "fix(pkg): tighten the bound\n\nbody line one\nbody line two"}},
		want:    "fix(pkg): tighten the bound\n\nbody line one\nbody line two",
	}, {
		name:   "two groups merge under the first headline",
		groups: []FanOutGroup{{Key: "skill/alpha"}, {Key: "skill/beta"}},
		results: []fakeResp{
			{msg: "fix(pkg): alpha headline\n\nalpha body"},
			{msg: "fix(pkg): beta headline\n\nbeta body"},
		},
		want: "fix(pkg): alpha headline\n\n" +
			"Applied by per-family fixer passes:\n" +
			"- skill/alpha: fix(pkg): alpha headline\n" +
			"- skill/beta: fix(pkg): beta headline\n",
	}, {
		name:   "empty key falls back to findings",
		groups: []FanOutGroup{{Key: ""}, {Key: "skill/beta"}},
		results: []fakeResp{
			{msg: "fix(pkg): first headline\n\nfirst body"},
			{msg: "fix(pkg): second headline"},
		},
		want: "fix(pkg): first headline\n\n" +
			"Applied by per-family fixer passes:\n" +
			"- findings: fix(pkg): first headline\n" +
			"- skill/beta: fix(pkg): second headline\n",
	}, {
		name:   "extra groups beyond results are not rendered",
		groups: []FanOutGroup{{Key: "skill/alpha"}, {Key: "skill/beta"}, {Key: "skill/gamma"}},
		results: []fakeResp{
			{msg: "fix(pkg): alpha headline"},
			{msg: "fix(pkg): beta headline"},
		},
		want: "fix(pkg): alpha headline\n\n" +
			"Applied by per-family fixer passes:\n" +
			"- skill/alpha: fix(pkg): alpha headline\n" +
			"- skill/beta: fix(pkg): beta headline\n",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := combineCommitMessages(tc.groups, tc.results)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("combineCommitMessages mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAppendRemainingNote(t *testing.T) {
	note := func(n int) string {
		return fmt.Sprintf("Local checks still report %d finding(s) after the verification cap; CI reports them on the pull request.", n)
	}
	review := func(id string) callbacks.Finding {
		return callbacks.Finding{Kind: callbacks.FindingKindReview, Identifier: id}
	}

	tests := []struct {
		name      string
		summary   string
		remaining []callbacks.Finding
		want      string
	}{{
		name:      "zero remaining appends note after summary",
		summary:   "did the work",
		remaining: nil,
		want:      "did the work\n\n" + note(0),
	}, {
		name:      "several remaining reports the count",
		summary:   "did the work",
		remaining: []callbacks.Finding{review("t1"), review("t2"), review("t3")},
		want:      "did the work\n\n" + note(3),
	}, {
		name:      "empty summary returns the note alone",
		summary:   "",
		remaining: []callbacks.Finding{review("t1")},
		want:      note(1),
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appendRemainingNote(tc.summary, tc.remaining)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("appendRemainingNote mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// fanCB is a stand-in callback set for the runAgentPasses harness.
type fanCB struct{}

// fanResult carries a commit message and a give-up explanation.
type fanResult struct {
	msg  string
	expl string
}

func (r *fanResult) GetCommitMessage() string       { return r.msg }
func (r *fanResult) GetNoChangeExplanation() string { return r.expl }

// fanReq opts into fan-out, local verification (rounds and the scripted
// per-call verify findings), and the once-before-commit finalizer, and records
// SetLocalFindings and Finalize calls.
type fanReq struct {
	findings  []callbacks.Finding
	groups    []FanOutGroup
	note      string
	finalized int
	rounds    int
	verify    [][]callbacks.Finding
}

func (r *fanReq) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) { return p, nil }
func (r *fanReq) FanOutGroups() []FanOutGroup                                 { return r.groups }
func (r *fanReq) SetLocalFindings(f []callbacks.Finding)                      { r.findings = f }
func (r *fanReq) MaxLocalRounds() int                                         { return r.rounds }

// LocalVerify returns the next scripted finding set, and none once the script
// is exhausted.
func (r *fanReq) LocalVerify(context.Context, *gogit.Worktree) []callbacks.Finding {
	if len(r.verify) == 0 {
		return nil
	}
	next := r.verify[0]
	r.verify = r.verify[1:]
	return next
}

func (r *fanReq) Finalize(context.Context, *gogit.Worktree) string {
	r.finalized++
	return r.note
}

// fanAgent returns a scripted result per Execute call. edits and errs, keyed
// by call index, make a call write files under root before it returns and make
// it fail — like an agent that edited files and then ran out of turns.
type fanAgent struct {
	perCall []*fanResult
	calls   int
	root    string
	edits   map[int]map[string]string
	errs    map[int]error
}

func (a *fanAgent) Execute(context.Context, *fanReq, fanCB) (*fanResult, error) {
	call := a.calls
	a.calls++
	for name, content := range a.edits[call] {
		full := filepath.Join(a.root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return nil, err
		}
	}
	if err := a.errs[call]; err != nil {
		return nil, err
	}
	return a.perCall[min(call, len(a.perCall)-1)], nil
}

func TestRunAgentPassesFanOut(t *testing.T) {
	t.Parallel()

	orig := []callbacks.Finding{finding("x"), finding("y")}
	req := &fanReq{
		findings: orig,
		groups: []FanOutGroup{
			{Key: "skill/a", Findings: []callbacks.Finding{finding("x")}},
			{Key: "skill/b", Findings: []callbacks.Finding{finding("y")}},
		},
		note: "Post-edit tooling notes:\n- go mod tidy also changed: go.sum",
	}
	agent := &fanAgent{perCall: []*fanResult{
		{msg: "fix: alpha\n\nbody a", expl: "no A change"},
		{msg: "fix: beta\n\nbody b", expl: "no B change"},
	}}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}

	outcome, err := r.runAgentPasses(t.Context(), nil, fanCB{}, req, orig)
	if err != nil {
		t.Fatalf("runAgentPasses: %v", err)
	}
	if agent.calls != 2 {
		t.Errorf("agent ran %d times, want one per group (2)", agent.calls)
	}
	// The finalizer runs once, and its note lands in the commit body.
	if req.finalized != 1 {
		t.Errorf("finalizer ran %d times, want 1", req.finalized)
	}
	if !strings.Contains(outcome.commitMessage, "Post-edit tooling notes:") {
		t.Errorf("commit message missing the finalizer note:\n%s", outcome.commitMessage)
	}
	// The persisted headline never carries the group key (it anchors the title);
	// the family is recorded in the body instead.
	if len(outcome.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(outcome.entries))
	}
	if outcome.entries[0].headline != "fix: alpha" {
		t.Errorf("headline = %q, want %q (no group-key prefix)", outcome.entries[0].headline, "fix: alpha")
	}
	if !strings.HasPrefix(outcome.entries[0].summary, "Family skill/a:") {
		t.Errorf("summary should record the family, got %q", outcome.entries[0].summary)
	}
	// The full finding set is restored so the PR body lists every finding
	// addressed rather than only the last pass's.
	if !slices.Equal(req.findings, orig) {
		t.Errorf("findings not restored after the passes: got %v, want %v", req.findings, orig)
	}
	// A no-change give-up aggregates every pass's explanation.
	if want := "no A change\n\nno B change"; outcome.giveUpExplanation != want {
		t.Errorf("giveUpExplanation = %q, want %q", outcome.giveUpExplanation, want)
	}
}

func TestBudgetChecker(t *testing.T) {
	t.Parallel()

	t.Run("no capability is unbounded", func(t *testing.T) {
		t.Parallel()
		ok := budgetChecker(t.Context(), struct{}{})
		if !ok() {
			t.Error("a request without the budget capability must be unbounded")
		}
	})

	t.Run("no deadline is unbounded", func(t *testing.T) {
		t.Parallel()
		ok := budgetChecker(t.Context(), &fanReqBudget{margin: time.Minute})
		if !ok() {
			t.Error("a context without a deadline must be unbounded")
		}
	})

	t.Run("within and beyond the margin", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		if ok := budgetChecker(ctx, &fanReqBudget{margin: time.Minute}); !ok() {
			t.Error("an hour left against a one-minute margin must be within budget")
		}
		if ok := budgetChecker(ctx, &fanReqBudget{margin: 2 * time.Hour}); ok() {
			t.Error("a two-hour margin against an hour left must be over budget")
		}
	})
}

// fanReqBudget opts into the wall-clock budget with a fixed margin.
type fanReqBudget struct{ margin time.Duration }

func (r *fanReqBudget) ReconcileMargin() time.Duration { return r.margin }

func TestAppendCommitNote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		note    string
		want    string
	}{{
		name:    "note appended after a blank line",
		message: "fix(pkg): do the thing\n\nbody",
		note:    "Post-edit tooling notes:\n- go mod tidy also changed: go.sum",
		want:    "fix(pkg): do the thing\n\nbody\n\nPost-edit tooling notes:\n- go mod tidy also changed: go.sum",
	}, {
		name:    "empty message yields the note alone",
		message: "",
		note:    "note",
		want:    "note",
	}, {
		name:    "trailing newlines trimmed before the note",
		message: "subject\n\n",
		note:    "note",
		want:    "subject\n\nnote",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := appendCommitNote(tc.message, tc.note); got != tc.want {
				t.Errorf("appendCommitNote() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunAgentPassesRevertsExhaustedGroup(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	all := []callbacks.Finding{finding("a"), finding("b1"), finding("b2"), finding("c")}
	req := &fanReq{
		findings: all,
		groups: []FanOutGroup{
			{Key: "skill/a", Findings: all[:1]},
			{Key: "skill/b", Findings: all[1:3]},
			{Key: "skill/c", Findings: all[3:]},
		},
	}
	agent := &fanAgent{
		root:    root,
		perCall: []*fanResult{{msg: "fix: alpha"}, {msg: "fix: beta"}, {msg: "fix: gamma"}},
		edits: map[int]map[string]string{
			0: {"a.txt": "a\n"},
			1: {"b.txt": "b\n", "base.txt": "clobbered\n"}, // written before the turn budget ran out
			2: {"c.txt": "c\n"},
		},
		errs: map[int]error{1: fmt.Errorf("%w (200)", executor.ErrMaxTurns)},
	}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}

	outcome, err := r.runAgentPasses(t.Context(), wt, fanCB{}, req, all)
	if err != nil {
		t.Fatalf("runAgentPasses: %v", err)
	}
	if agent.calls != 3 {
		t.Errorf("agent ran %d times, want 3: the groups after an exhausted one still run", agent.calls)
	}
	// The completed passes' edits stay; the exhausted pass's edits, including
	// its change to a committed file, are gone.
	requireCheckout(t, root, map[string]string{"a.txt": "a\n", "c.txt": "c\n", "base.txt": "base\n"}, []string{"b.txt"})
	if len(outcome.entries) != 2 {
		t.Fatalf("entries = %d, want one per completed pass (2)", len(outcome.entries))
	}
	// The commit body pairs each completed family with its own headline and
	// records the exhausted family separately.
	for _, want := range []string{"- skill/a: fix: alpha", "- skill/c: fix: gamma", "skill/b: 2 finding(s)"} {
		if !strings.Contains(outcome.commitMessage, want) {
			t.Errorf("commit message missing %q:\n%s", want, outcome.commitMessage)
		}
	}
	if strings.Contains(outcome.commitMessage, "- skill/b: fix") {
		t.Errorf("commit message credits the exhausted family with a completed pass:\n%s", outcome.commitMessage)
	}
	if !slices.Equal(req.findings, all) {
		t.Errorf("findings not restored after the passes: got %v, want %v", req.findings, all)
	}
}

func TestRunAgentPassesAllGroupsExhaustedIsAnExplainedNoChange(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	all := []callbacks.Finding{finding("a"), finding("b")}
	req := &fanReq{
		findings: all,
		groups:   []FanOutGroup{{Key: "skill/a", Findings: all[:1]}, {Key: "skill/b", Findings: all[1:]}},
	}
	agent := &fanAgent{
		root:    root,
		perCall: []*fanResult{{msg: "fix: never"}},
		edits:   map[int]map[string]string{0: {"a.txt": "a\n"}, 1: {"base.txt": "clobbered\n"}},
		errs:    map[int]error{0: executor.ErrMaxTurns, 1: executor.ErrMaxTurns},
	}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}

	outcome, err := r.runAgentPasses(t.Context(), wt, fanCB{}, req, all)
	if err != nil {
		t.Fatalf("runAgentPasses: %v", err)
	}
	status, err := wt.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.IsClean() {
		t.Errorf("checkout should be clean when every pass was reverted: %v", status)
	}
	if len(outcome.entries) != 0 || outcome.commitMessage != "" {
		t.Errorf("outcome should carry nothing to commit, got entries=%d message=%q", len(outcome.entries), outcome.commitMessage)
	}
	for _, want := range []string{"Every fixer pass exhausted its turn budget", "skill/a: 1 finding(s)", "skill/b: 1 finding(s)"} {
		if !strings.Contains(outcome.giveUpExplanation, want) {
			t.Errorf("give-up explanation missing %q:\n%s", want, outcome.giveUpExplanation)
		}
	}
	if req.finalized != 0 {
		t.Errorf("finalizer ran %d times, want 0 when nothing is committed", req.finalized)
	}
}

func TestRunAgentPassesMaxTurnsWithoutCheckoutStillFails(t *testing.T) {
	t.Parallel()
	all := []callbacks.Finding{finding("a")}
	req := &fanReq{findings: all, groups: []FanOutGroup{{Key: "skill/a", Findings: all}}}
	agent := &fanAgent{perCall: []*fanResult{{msg: "fix: never"}}, errs: map[int]error{0: executor.ErrMaxTurns}}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}

	_, err := r.runAgentPasses(t.Context(), nil, fanCB{}, req, all)
	if !errors.Is(err, executor.ErrMaxTurns) {
		t.Errorf("error: got = %v, want the turn-limit error when there is no checkout to revert", err)
	}
}

func TestRunAgentPassesOtherErrorsStillFail(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	all := []callbacks.Finding{finding("a")}
	req := &fanReq{findings: all, groups: []FanOutGroup{{Key: "skill/a", Findings: all}}}
	boom := errors.New("provider unavailable")
	agent := &fanAgent{root: root, perCall: []*fanResult{{msg: "fix: never"}}, errs: map[int]error{0: boom}}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}

	_, err := r.runAgentPasses(t.Context(), wt, fanCB{}, req, all)
	if !errors.Is(err, boom) {
		t.Errorf("error: got = %v, want the agent error propagated unchanged", err)
	}
}

func TestVerifyPassRevertsExhaustedReRun(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	// The pass itself already ran and wrote a.txt; the re-run is what runs here.
	writeCheckoutFile(t, root, "a.txt", "a\n")
	req := &fanReq{rounds: 2, verify: [][]callbacks.Finding{{finding("lint")}}}
	agent := &fanAgent{
		root:    root,
		perCall: []*fanResult{{msg: "fix: rerun"}},
		edits:   map[int]map[string]string{0: {"junk.txt": "half-done\n", "a.txt": "mangled\n"}},
		errs:    map[int]error{0: executor.ErrMaxTurns},
	}
	r := &PRReconciler[*fanReq, *fanResult, fanCB]{agent: agent}
	initial := &fanResult{msg: "fix: pass"}

	result, summaries, remaining, err := r.verifyPass(t.Context(), wt, fanCB{}, req, initial, nil)
	if err != nil {
		t.Fatalf("verifyPass: %v", err)
	}
	if result != initial {
		t.Errorf("result = %+v, want the pass's own result kept", result)
	}
	if agent.calls != 1 {
		t.Errorf("agent ran %d times, want 1: no further round after a reverted re-run", agent.calls)
	}
	requireCheckout(t, root, map[string]string{"a.txt": "a\n", "base.txt": "base\n"}, []string{"junk.txt"})
	if len(remaining) != 1 {
		t.Errorf("remaining = %d, want the finding the reverted re-run was addressing", len(remaining))
	}
	if !slices.Contains(summaries, reRunRevertedSummary) {
		t.Errorf("summaries %q should record the reverted re-run", summaries)
	}
}
