/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	gogit "github.com/go-git/go-git/v5"
	"github.com/google/go-cmp/cmp"
)

func TestIssueDataEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b IssueData
		want bool
	}{{
		name: "same key matches despite different content",
		a:    IssueData{Key: "modernize", Path: "a", Diagnostics: []Diagnostic{{Rule: "modernize", Line: 1}}},
		b:    IssueData{Key: "modernize", Path: "b", Diagnostics: []Diagnostic{{Rule: "modernize", Line: 9}}},
		want: true,
	}, {
		name: "different keys do not match",
		a:    IssueData{Key: "modernize"},
		b:    IssueData{Key: "gofmt"},
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("Equal: got = %v, want = %v", got, tt.want)
			}
		})
	}
}

func TestGroupByRule(t *testing.T) {
	res := &githubreconciler.Resource{Path: "pkg/thing"}
	diags := []Diagnostic{
		{Path: "pkg/thing/b.go", Line: 9, Rule: "modernize", Message: "z"},
		{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "y"},
		{Path: "pkg/thing/a.go", Line: 1, Rule: "modernize", Message: "x"},
	}

	got := GroupByRule(res, diags)

	want := []*IssueData{{
		Key:  "gofmt",
		Path: "pkg/thing",
		Rule: "gofmt",
		Diagnostics: []Diagnostic{
			{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "y"},
		},
	}, {
		Key:  "modernize",
		Path: "pkg/thing",
		Rule: "modernize",
		Diagnostics: []Diagnostic{
			{Path: "pkg/thing/a.go", Line: 1, Rule: "modernize", Message: "x"},
			{Path: "pkg/thing/b.go", Line: 9, Rule: "modernize", Message: "z"},
		},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GroupByRule (-want, +got):\n%s", diff)
	}

	if got := GroupByRule(res, nil); len(got) != 0 {
		t.Errorf("GroupByRule(nil): got = %d issues, want = 0", len(got))
	}
}

func TestGroupByPath(t *testing.T) {
	res := &githubreconciler.Resource{Path: "pkg/thing"}
	diags := []Diagnostic{
		{Path: "pkg/thing/b.go", Line: 9, Rule: "modernize", Message: "z"},
		{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "y"},
	}

	got := GroupByPath(res, diags)

	want := []*IssueData{{
		Key:  "pkg/thing",
		Path: "pkg/thing",
		Diagnostics: []Diagnostic{
			{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "y"},
			{Path: "pkg/thing/b.go", Line: 9, Rule: "modernize", Message: "z"},
		},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GroupByPath (-want, +got):\n%s", diff)
	}

	if got := GroupByPath(res, nil); got != nil {
		t.Errorf("GroupByPath(nil): got = %v, want = nil", got)
	}
}

func TestDeriveIssues(t *testing.T) {
	res := &githubreconciler.Resource{Path: "pkg/thing"}
	diags := []Diagnostic{
		{Path: "pkg/thing/a.go", Line: 1, Rule: "modernize", Message: "x"},
		{Path: "pkg/thing/a.go", Line: 3, Rule: "gofmt", Message: "y"},
	}

	desired, gotDiags, err := deriveIssues(t.Context(), &fakeAnalyzer{diagnostics: diags}, GroupByRule, res, nil, nil)
	if err != nil {
		t.Fatalf("deriveIssues: got = %v, want = nil", err)
	}
	if diff := cmp.Diff(diags, gotDiags); diff != "" {
		t.Errorf("diagnostics (-want, +got):\n%s", diff)
	}
	wantKeys := []string{"gofmt", "modernize"}
	if len(desired) != len(wantKeys) {
		t.Fatalf("desired: got = %d issues, want = %d", len(desired), len(wantKeys))
	}
	for i, want := range wantKeys {
		if desired[i].Key != want {
			t.Errorf("desired[%d].Key: got = %q, want = %q", i, desired[i].Key, want)
		}
	}
}

func TestDeriveIssuesError(t *testing.T) {
	wantErr := errors.New("analyze failed")
	if _, _, err := deriveIssues(t.Context(), &fakeAnalyzer{err: wantErr}, GroupByRule, &githubreconciler.Resource{Path: "p"}, nil, nil); !errors.Is(err, wantErr) {
		t.Errorf("deriveIssues error: got = %v, want = %v", err, wantErr)
	}
}

func TestDeriveIssuesDuplicateKeys(t *testing.T) {
	_, _, err := deriveIssues(t.Context(), &fakeAnalyzer{diagnostics: []Diagnostic{{Rule: "modernize"}}},
		func(*githubreconciler.Resource, []Diagnostic) []*IssueData {
			return []*IssueData{{Key: "dup"}, {Key: "dup"}}
		},
		&githubreconciler.Resource{Path: "p"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `duplicate key "dup"`) {
		t.Errorf("deriveIssues error: got = %v, want duplicate-key error", err)
	}
}

func TestDeriveIssuesCustomGrouping(t *testing.T) {
	var calls int
	desired, _, err := deriveIssues(t.Context(), &fakeAnalyzer{diagnostics: []Diagnostic{{Rule: "a"}, {Rule: "b"}}},
		func(res *githubreconciler.Resource, diags []Diagnostic) []*IssueData {
			calls++
			return GroupByPath(res, diags)
		},
		&githubreconciler.Resource{Path: "pkg/thing"}, nil, nil)
	if err != nil {
		t.Fatalf("deriveIssues: got = %v, want = nil", err)
	}
	if calls != 1 {
		t.Errorf("grouping calls: got = %d, want = 1", calls)
	}
	if len(desired) != 1 || desired[0].Key != "pkg/thing" {
		t.Errorf("desired: got = %+v, want single issue keyed pkg/thing", desired)
	}
}

func TestDeriveIssuesDropsFixedDiagnostics(t *testing.T) {
	desired, gotDiags, err := deriveIssues(t.Context(), &fakeAnalyzer{diagnostics: []Diagnostic{
		{Path: "a.go", Rule: "modernize", Message: "x"},
		{Path: "b.go", Rule: "image-main-package", Message: "informational", Fixed: true},
	}}, GroupByRule, &githubreconciler.Resource{Path: "p"}, nil, nil)
	if err != nil {
		t.Fatalf("deriveIssues: got = %v, want = nil", err)
	}
	if len(desired) != 1 || desired[0].Key != "modernize" {
		t.Errorf("desired: got = %+v, want single modernize issue", desired)
	}
	for _, d := range gotDiags {
		if d.Fixed {
			t.Errorf("diagnostics: got Fixed diagnostic %+v, want none", d)
		}
	}
}

func TestPriorFirst(t *testing.T) {
	desired := []*IssueData{
		{Key: "new-a"},
		{Key: "tracked-b"},
		{Key: "new-c"},
		{Key: "tracked-d"},
	}
	prior := []IssueData{{Key: "tracked-b"}, {Key: "tracked-d"}}

	got, tracked := priorFirst(desired, prior)

	if tracked != 2 {
		t.Errorf("tracked: got = %d, want = 2", tracked)
	}
	want := []string{"tracked-b", "tracked-d", "new-a", "new-c"}
	if len(got) != len(want) {
		t.Fatalf("priorFirst len: got = %d, want = %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Errorf("priorFirst[%d]: got = %q, want = %q", i, got[i].Key, k)
		}
	}

	// No prior issues: order is preserved.
	got, tracked = priorFirst(desired, nil)
	if tracked != 0 {
		t.Errorf("tracked(no prior): got = %d, want = 0", tracked)
	}
	for i := range desired {
		if got[i] != desired[i] {
			t.Errorf("priorFirst(no prior)[%d]: got = %v, want = %v", i, got[i], desired[i])
		}
	}
}

func TestTruncateDesired(t *testing.T) {
	prior := []IssueData{{Key: "tracked-a"}, {Key: "tracked-b"}, {Key: "tracked-c"}}

	tests := []struct {
		name    string
		desired []*IssueData
		limit   int
		want    []string
	}{{
		name: "sheds only net-new findings",
		desired: []*IssueData{
			{Key: "new-1"},
			{Key: "tracked-a"},
			{Key: "new-2"},
		},
		limit: 2,
		want:  []string{"tracked-a", "new-1"},
	}, {
		name: "tracked entries always survive a smaller cap",
		desired: []*IssueData{
			{Key: "new-1"},
			{Key: "tracked-a"},
			{Key: "tracked-b"},
			{Key: "tracked-c"},
		},
		limit: 1,
		want:  []string{"tracked-a", "tracked-b", "tracked-c"},
	}, {
		name: "all net-new truncates to the cap",
		desired: []*IssueData{
			{Key: "new-1"},
			{Key: "new-2"},
			{Key: "new-3"},
		},
		limit: 2,
		want:  []string{"new-1", "new-2"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateDesired(tt.desired, prior, tt.limit)
			if len(got) != len(tt.want) {
				t.Fatalf("truncateDesired len: got = %d, want = %d", len(got), len(tt.want))
			}
			for i, k := range tt.want {
				if got[i].Key != k {
					t.Errorf("truncateDesired[%d]: got = %q, want = %q", i, got[i].Key, k)
				}
			}
		})
	}
}

func TestIssueReconcilerReviewOnlySkipsPath(t *testing.T) {
	rec := &IssueReconciler{
		core: core{
			identity: "test-identity",
			mode:     ModeReview,
		},
	}

	err := rec.Reconcile(t.Context(), &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: "owner",
		Repo:  "repo",
	}, nil)
	if err != nil {
		t.Fatalf("Reconcile: got = %v, want = nil", err)
	}
}

func TestIssueReconcilerNoneModeSkipsPath(t *testing.T) {
	rec := &IssueReconciler{
		core: core{
			identity: "test-identity",
			mode:     ModeNone,
		},
	}

	err := rec.Reconcile(t.Context(), &githubreconciler.Resource{
		Type:  githubreconciler.ResourceTypePath,
		Owner: "owner",
		Repo:  "repo",
	}, nil)
	if err != nil {
		t.Fatalf("Reconcile: got = %v, want = nil", err)
	}
}

func TestWithGrouping(t *testing.T) {
	var o issuesOptions
	WithGrouping(GroupByPath).applyIssues(&o)
	if o.grouping == nil {
		t.Fatal("grouping: got = nil, want non-nil")
	}
	got := o.grouping(&githubreconciler.Resource{Path: "pkg/thing"}, []Diagnostic{{Rule: "a"}})
	if len(got) != 1 || got[0].Key != "pkg/thing" {
		t.Errorf("grouping: got = %+v, want single issue keyed pkg/thing", got)
	}
}

func TestWithCloseMessage(t *testing.T) {
	want := fmt.Sprintf("resolved-%d", rand.Int64())
	var o issuesOptions
	WithCloseMessage(want).applyIssues(&o)
	if o.closeMessage != want {
		t.Errorf("closeMessage: got = %q, want = %q", o.closeMessage, want)
	}
}

func TestSharedOptionsApplyToIssues(t *testing.T) {
	var o issuesOptions
	WithMode(ModeAll).applyIssues(&o)
	WithLabels("one", "two").applyIssues(&o)
	WithLabels("three").applyIssues(&o)
	WithLabelFunc(func(context.Context, *githubreconciler.Resource, []Diagnostic, []callbacks.Finding) []string {
		return nil
	}).applyIssues(&o)

	if o.mode != ModeAll {
		t.Errorf("mode: got = %v, want = %v", o.mode, ModeAll)
	}
	want := []string{"one", "two", "three"}
	if diff := cmp.Diff(want, o.labels); diff != "" {
		t.Errorf("labels (-want, +got):\n%s", diff)
	}
	if o.labelFn == nil {
		t.Error("labelFn: got = nil, want non-nil")
	}
}

func TestSharedOptionsApplyToPR(t *testing.T) {
	var o prOptions
	WithLabels("one", "two").applyPR(&o)
	want := []string{"one", "two"}
	if diff := cmp.Diff(want, o.labels); diff != "" {
		t.Errorf("labels (-want, +got):\n%s", diff)
	}
}

func TestEnsureCleanWorktree(t *testing.T) {
	wt := newTestWorktree(t)
	if err := ensureCleanWorktree(wt); err != nil {
		t.Fatalf("clean worktree: got = %v, want = nil", err)
	}

	f, err := wt.Filesystem.Create("dirty.txt")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := f.Write([]byte("mutation")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if err := ensureCleanWorktree(wt); err == nil {
		t.Error("dirty worktree: got = nil, want error")
	}
}

// analyzeFunc adapts a function into an Analyzer for tests.
type analyzeFunc func(context.Context, *gogit.Worktree, []string, ...Diagnostic) ([]Diagnostic, error)

func (f analyzeFunc) Analyze(ctx context.Context, wt *gogit.Worktree, paths []string, prior ...Diagnostic) ([]Diagnostic, error) {
	return f(ctx, wt, paths, prior...)
}

func TestDeriveIssuesForwardsPrior(t *testing.T) {
	prior := []Diagnostic{{Rule: "modernize", Path: "a.go", Message: "tracked"}}
	var got []Diagnostic
	a := analyzeFunc(func(_ context.Context, _ *gogit.Worktree, _ []string, p ...Diagnostic) ([]Diagnostic, error) {
		got = p
		return nil, nil
	})
	if _, _, err := deriveIssues(t.Context(), a, GroupByRule, &githubreconciler.Resource{Path: "p"}, nil, prior); err != nil {
		t.Fatalf("deriveIssues: got = %v, want = nil", err)
	}
	if diff := cmp.Diff(prior, got); diff != "" {
		t.Errorf("prior (-want, +got):\n%s", diff)
	}
}
