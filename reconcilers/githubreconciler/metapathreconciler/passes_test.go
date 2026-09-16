/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

// fanReq opts into fan-out, local verification (disabled here), and the
// once-before-commit finalizer, and records SetLocalFindings and Finalize calls.
type fanReq struct {
	findings  []callbacks.Finding
	groups    []FanOutGroup
	note      string
	finalized int
}

func (r *fanReq) Bind(p *promptbuilder.Prompt) (*promptbuilder.Prompt, error) { return p, nil }
func (r *fanReq) FanOutGroups() []FanOutGroup                                 { return r.groups }
func (r *fanReq) SetLocalFindings(f []callbacks.Finding)                      { r.findings = f }
func (r *fanReq) MaxLocalRounds() int                                         { return 0 }
func (r *fanReq) LocalVerify(context.Context, *gogit.Worktree) []callbacks.Finding {
	return nil
}

func (r *fanReq) Finalize(context.Context, *gogit.Worktree) string {
	r.finalized++
	return r.note
}

// fanAgent returns a scripted result per Execute call.
type fanAgent struct {
	perCall []*fanResult
	calls   int
}

func (a *fanAgent) Execute(context.Context, *fanReq, fanCB) (*fanResult, error) {
	res := a.perCall[min(a.calls, len(a.perCall)-1)]
	a.calls++
	return res, nil
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
