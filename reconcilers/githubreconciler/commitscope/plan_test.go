/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"slices"
	"testing"
)

// stagedPaths returns the staged paths for concise assertions.
func stagedPaths(r Result) []string {
	out := make([]string, 0, len(r.Staged))
	for _, c := range r.Staged {
		out = append(out, c.Path)
	}
	slices.Sort(out)
	return out
}

func droppedPaths(r Result) []string {
	out := make([]string, 0, len(r.Dropped))
	for _, d := range r.Dropped {
		out = append(out, d.Path)
	}
	slices.Sort(out)
	return out
}

func TestPlan(t *testing.T) {
	dl := DefaultDenylist()
	tests := []struct {
		name        string
		changes     []Change
		wantStaged  []string
		wantDropped []string
	}{
		{
			name: "intended source kept, unintended dropped",
			changes: []Change{
				{Path: "a.go", Type: Added, Intended: true},
				{Path: "b.go", Type: Added, Intended: false},
			},
			wantStaged:  []string{"a.go"},
			wantDropped: []string{"b.go"},
		},
		{
			name: "intended go.sum kept",
			changes: []Change{
				{Path: "go.sum", Type: Modified, Intended: true, TrackedAtBase: true},
			},
			wantStaged:  []string{"go.sum"},
			wantDropped: []string{},
		},
		{
			name: "agent-written file that is also denylisted is dropped",
			changes: []Change{
				{Path: "pkg/foo.test", Type: Added, Intended: true, IsBinary: true},
			},
			wantStaged:  []string{},
			wantDropped: []string{"pkg/foo.test"},
		},
		{
			name: "only denylisted changes yield nothing staged",
			changes: []Change{
				{Path: ".terraform/x", Type: Added, Intended: true},
				{Path: "app.test", Type: Added, Intended: true},
			},
			wantStaged:  []string{},
			wantDropped: []string{".terraform/x", "app.test"},
		},
		{
			name:        "empty intent yields no commit",
			changes:     []Change{{Path: "junk", Type: Added, Intended: false}},
			wantStaged:  []string{},
			wantDropped: []string{"junk"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(tc.changes, dl)
			if diff := slices.Compare(stagedPaths(got), tc.wantStaged); diff != 0 {
				t.Errorf("staged: got = %v, want = %v", stagedPaths(got), tc.wantStaged)
			}
			if diff := slices.Compare(droppedPaths(got), tc.wantDropped); diff != 0 {
				t.Errorf("dropped: got = %v, want = %v", droppedPaths(got), tc.wantDropped)
			}
		})
	}
}

// TestPlanDenylistMonotone confirms the guard postcondition that a stricter
// denylist never adds a staged path.
func TestPlanDenylistMonotone(t *testing.T) {
	changes := []Change{
		{Path: "a.go", Type: Added, Intended: true},
		{Path: "big.json", Type: Added, Intended: true, Size: 2000},
	}
	loose := Denylist{MaxFileSize: 0}
	strict := Denylist{MaxFileSize: 1000}

	looseStaged := stagedPaths(Plan(changes, loose))
	strictStaged := stagedPaths(Plan(changes, strict))

	for _, p := range strictStaged {
		if !slices.Contains(looseStaged, p) {
			t.Errorf("stricter denylist added staged path %q (loose = %v, strict = %v)", p, looseStaged, strictStaged)
		}
	}
	if slices.Contains(strictStaged, "big.json") {
		t.Errorf("strict denylist staged oversize file: %v", strictStaged)
	}
}

// TestPlanCommittedSubsetIntended confirms every staged change was intended and
// not denied, the core guard invariant.
func TestPlanCommittedSubsetIntended(t *testing.T) {
	dl := DefaultDenylist()
	changes := []Change{
		{Path: "a.go", Type: Added, Intended: true},
		{Path: "b.test", Type: Added, Intended: true, IsBinary: true},
		{Path: "c.go", Type: Added, Intended: false},
		{Path: "go.sum", Type: Modified, Intended: true, TrackedAtBase: true},
	}
	res := Plan(changes, dl)
	for _, c := range res.Staged {
		if !c.Intended {
			t.Errorf("staged unintended change %q", c.Path)
		}
		if deny, reason := dl.Denied(c); deny {
			t.Errorf("staged denied change %q (%s)", c.Path, reason)
		}
	}
}
