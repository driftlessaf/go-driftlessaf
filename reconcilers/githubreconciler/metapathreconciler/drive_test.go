/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"errors"
	"slices"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

func finding(id string) callbacks.Finding {
	return callbacks.Finding{Kind: callbacks.FindingKindCICheck, Identifier: id, Name: id, Details: id}
}

func TestRunVerificationOneExtraRoundThenClean(t *testing.T) {
	t.Parallel()

	verifyReturns := [][]callbacks.Finding{{finding("f1")}, nil}
	call := 0
	verify := func() []callbacks.Finding {
		out := verifyReturns[min(call, len(verifyReturns)-1)]
		call++
		return out
	}
	var applied [][]callbacks.Finding
	runs := 0
	result, rounds, summaries, remaining, err := runVerification(
		verify,
		func(f []callbacks.Finding) { applied = append(applied, f) },
		func() (string, string, error) { runs++; return "round", "did f1", nil },
		2, "initial", nil,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rounds != 1 {
		t.Errorf("rounds = %d, want 1", rounds)
	}
	if runs != 1 {
		t.Errorf("agent runs = %d, want 1", runs)
	}
	if result != "round" {
		t.Errorf("result = %q, want %q", result, "round")
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want none", remaining)
	}
	if want := []string{"did f1"}; !slices.Equal(summaries, want) {
		t.Errorf("summaries = %v, want %v (one per round)", summaries, want)
	}
	if len(applied) != 1 || applied[0][0].Identifier != "f1" {
		t.Errorf("applied = %v, want the verification findings once", applied)
	}
}

func TestRunVerificationCapHonored(t *testing.T) {
	t.Parallel()

	runs := 0
	result, rounds, summaries, remaining, err := runVerification(
		func() []callbacks.Finding { return []callbacks.Finding{finding("stubborn")} },
		func([]callbacks.Finding) {},
		func() (string, string, error) { runs++; return "again", "retry", nil },
		2, "initial", nil,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rounds != 2 {
		t.Errorf("rounds = %d, want 2 (cap)", rounds)
	}
	if runs != 2 {
		t.Errorf("agent runs = %d, want 2", runs)
	}
	if result != "again" {
		t.Errorf("result = %q, want %q", result, "again")
	}
	if len(summaries) != 2 {
		t.Errorf("summaries = %v, want 2 (one per round)", summaries)
	}
	if len(remaining) != 1 || remaining[0].Identifier != "stubborn" {
		t.Errorf("remaining = %v, want the still-open finding", remaining)
	}
}

func TestRunVerificationCleanTriggersNoRun(t *testing.T) {
	t.Parallel()

	runs := 0
	_, rounds, summaries, remaining, err := runVerification(
		func() []callbacks.Finding { return nil },
		func([]callbacks.Finding) { t.Fatal("apply should not fire on a clean result") },
		func() (string, string, error) { runs++; return "x", "", nil },
		2, "initial", nil,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rounds != 0 || runs != 0 {
		t.Errorf("rounds = %d runs = %d, want 0 and 0", rounds, runs)
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	if summaries != nil {
		t.Errorf("summaries = %v, want nil", summaries)
	}
}

// A failed re-run has already written partial edits through the checkout, so the
// error must propagate (not be swallowed): the caller aborts before any commit
// rather than committing the partial edits alongside the previous good result.
func TestRunVerificationReRunFailureReturnsError(t *testing.T) {
	t.Parallel()

	boom := errors.New("agent re-run failed")
	result, rounds, summaries, remaining, err := runVerification(
		func() []callbacks.Finding { return []callbacks.Finding{finding("f")} },
		func([]callbacks.Finding) {},
		func() (string, string, error) { return "", "", boom },
		2, "initial", nil,
	)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v (propagated, not swallowed)", err, boom)
	}
	if rounds != 1 {
		t.Errorf("rounds = %d, want 1 (one attempt)", rounds)
	}
	if result != "initial" {
		t.Errorf("result = %q, want the pre-failure result %q", result, "initial")
	}
	if len(remaining) != 1 {
		t.Errorf("remaining = %v, want the finding that was being fixed", remaining)
	}
	if summaries != nil {
		t.Errorf("summaries = %v, want nil", summaries)
	}
}

// Once the wall-clock budget is spent the loop starts no new round: it neither
// verifies nor re-runs, so finished work is left intact for the commit.
func TestRunVerificationStopsWhenOverBudget(t *testing.T) {
	t.Parallel()

	runs := 0
	result, rounds, _, remaining, err := runVerification(
		func() []callbacks.Finding { t.Fatal("verify should not fire once over budget"); return nil },
		func([]callbacks.Finding) {},
		func() (string, string, error) { runs++; return "x", "", nil },
		3, "initial", func() bool { return false },
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rounds != 0 || runs != 0 {
		t.Errorf("rounds = %d runs = %d, want 0 and 0", rounds, runs)
	}
	if result != "initial" {
		t.Errorf("result = %q, want %q", result, "initial")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
}

func TestRunFanOutOneInvocationPerGroupInOrder(t *testing.T) {
	t.Parallel()

	groups := []FanOutGroup{
		{Key: "a", Findings: []callbacks.Finding{finding("a1")}},
		{Key: "b", Findings: []callbacks.Finding{finding("b1"), finding("b2")}},
		{Key: "c", Findings: []callbacks.Finding{finding("c1")}},
	}
	var appliedCounts []int
	var ran []string
	results, err := runFanOut(
		groups,
		func(f []callbacks.Finding) { appliedCounts = append(appliedCounts, len(f)) },
		func(g FanOutGroup) (string, error) { ran = append(ran, g.Key); return g.Key, nil },
	)
	if err != nil {
		t.Fatalf("runFanOut: %v", err)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(ran, want) {
		t.Errorf("ran groups = %v, want %v", ran, want)
	}
	if want := []int{1, 2, 1}; !slices.Equal(appliedCounts, want) {
		t.Errorf("applied finding counts = %v, want %v (each group's own subset)", appliedCounts, want)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(results, want) {
		t.Errorf("results = %v, want %v", results, want)
	}
}

func TestRunFanOutGroupErrorStops(t *testing.T) {
	t.Parallel()

	boom := errors.New("family b failed")
	groups := []FanOutGroup{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	results, err := runFanOut(
		groups,
		func([]callbacks.Finding) {},
		func(g FanOutGroup) (string, error) {
			if g.Key == "b" {
				return "", boom
			}
			return g.Key, nil
		},
	)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if want := []string{"a"}; !slices.Equal(results, want) {
		t.Errorf("results before the failure = %v, want %v", results, want)
	}
}

// fanOutRequest is a request stand-in that opts into fan-out.
type fanOutRequest struct {
	groups []FanOutGroup
	set    [][]callbacks.Finding
}

func (r *fanOutRequest) FanOutGroups() []FanOutGroup            { return r.groups }
func (r *fanOutRequest) SetLocalFindings(f []callbacks.Finding) { r.set = append(r.set, f) }

func TestGroupsForRequest(t *testing.T) {
	t.Parallel()

	all := []callbacks.Finding{finding("x"), finding("y")}

	t.Run("no capability yields a single default group", func(t *testing.T) {
		t.Parallel()
		got := groupsForRequest(struct{}{}, all)
		if len(got) != 1 || !slices.Equal(got[0].Findings, all) {
			t.Errorf("got %v, want one group carrying all findings", got)
		}
	})

	t.Run("switch off (one group) restores a single run", func(t *testing.T) {
		t.Parallel()
		req := &fanOutRequest{groups: []FanOutGroup{{Key: "only", Findings: all}}}
		got := groupsForRequest(req, all)
		if len(got) != 1 || got[0].Key != "only" {
			t.Errorf("got %v, want the single opted-in group", got)
		}
	})

	t.Run("empty groups fall back to the default", func(t *testing.T) {
		t.Parallel()
		req := &fanOutRequest{groups: nil}
		got := groupsForRequest(req, all)
		if len(got) != 1 || !slices.Equal(got[0].Findings, all) {
			t.Errorf("got %v, want the default group", got)
		}
	})

	t.Run("multiple groups are used as given", func(t *testing.T) {
		t.Parallel()
		req := &fanOutRequest{groups: []FanOutGroup{{Key: "a"}, {Key: "b"}}}
		got := groupsForRequest(req, all)
		if len(got) != 2 {
			t.Errorf("got %d groups, want 2", len(got))
		}
	})
}
