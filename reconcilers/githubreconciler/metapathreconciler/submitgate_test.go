/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/clonemanager"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// reviewCall records one Analyze invocation.
type reviewCall struct {
	paths []string
	prior []Diagnostic
}

// scriptedReviewer returns its scripted results call by call and records
// what it was asked to review.
type scriptedReviewer struct {
	mu      sync.Mutex
	calls   []reviewCall
	results [][]Diagnostic
	err     error
}

func (s *scriptedReviewer) Analyze(_ context.Context, _ *gogit.Worktree, paths []string, prior ...Diagnostic) ([]Diagnostic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, reviewCall{paths: slices.Clone(paths), prior: slices.Clone(prior)})
	if s.err != nil {
		return nil, s.err
	}
	if n := len(s.calls) - 1; n < len(s.results) {
		return s.results[n], nil
	}
	return nil, nil
}

// gateWorktree builds a repo with committed files a.txt through f.txt, then
// touches them so both sides of the status split are represented: an
// unstaged modification (a.txt), an unstaged deletion (b.txt), an untracked
// file (d.txt), a staged modification (e.txt — the shape agent edits take,
// since clonemanager's WorktreeCallbacks stage as they write), and a staged
// deletion (f.txt). The changed scope the gate should compute is
// [a.txt d.txt e.txt].
func gateWorktree(t *testing.T) *gogit.Worktree {
	t.Helper()

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	for _, name := range []string{"a.txt", "b.txt", "c.txt", "e.txt", "f.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("original\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("Add(%s): %v", name, err)
		}
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(a.txt): %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("Remove(b.txt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(d.txt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "e.txt"), []byte("modified and staged\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(e.txt): %v", err)
	}
	if _, err := wt.Add("e.txt"); err != nil {
		t.Fatalf("Add(e.txt): %v", err)
	}
	if _, err := wt.Remove("f.txt"); err != nil {
		t.Fatalf("Remove(f.txt): %v", err)
	}
	return wt
}

func diag(rule, path, message string) Diagnostic {
	return Diagnostic{Rule: rule, Path: path, Line: 1, Message: message}
}

func TestSubmitGateOutsideLeaseAccepts(t *testing.T) {
	t.Parallel()

	reviewer := &scriptedReviewer{results: [][]Diagnostic{{diag("rule", "a.txt", "boom")}}}
	findings, err := SubmitGate[*struct{}](reviewer)(t.Context(), nil, "reasoning")
	if err != nil {
		t.Fatalf("SubmitGate: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %v, want = none", findings)
	}
	if len(reviewer.calls) != 0 {
		t.Errorf("reviewer calls: got = %d, want = 0", len(reviewer.calls))
	}
}

func TestSubmitGateCleanWorktreeAccepts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wt.Add("a.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	reviewer := &scriptedReviewer{results: [][]Diagnostic{{diag("rule", "a.txt", "boom")}}}
	ctx := clonemanager.WithWorktree(t.Context(), wt)
	findings, err := SubmitGate[*struct{}](reviewer)(ctx, nil, "reasoning")
	if err != nil {
		t.Fatalf("SubmitGate: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings: got = %v, want = none", findings)
	}
	if len(reviewer.calls) != 0 {
		t.Errorf("reviewer calls: got = %d, want = 0", len(reviewer.calls))
	}
}

func TestSubmitGateScopesToChangedPaths(t *testing.T) {
	t.Parallel()

	reviewer := &scriptedReviewer{}
	ctx := clonemanager.WithWorktree(t.Context(), gateWorktree(t))
	if _, err := SubmitGate[*struct{}](reviewer)(ctx, nil, "reasoning"); err != nil {
		t.Fatalf("SubmitGate: got = %v, want = nil", err)
	}
	if len(reviewer.calls) != 1 {
		t.Fatalf("reviewer calls: got = %d, want = 1", len(reviewer.calls))
	}
	// Unstaged and staged modifications plus the untracked file; both the
	// unstaged (b.txt) and staged (f.txt) deletions are excluded.
	want := []string{"a.txt", "d.txt", "e.txt"}
	if !slices.Equal(reviewer.calls[0].paths, want) {
		t.Errorf("paths: got = %v, want = %v", reviewer.calls[0].paths, want)
	}
	if len(reviewer.calls[0].prior) != 0 {
		t.Errorf("prior: got = %v, want = none", reviewer.calls[0].prior)
	}
}

func TestSubmitGateBlocksUnfixedEveryEvaluation(t *testing.T) {
	t.Parallel()

	reviewer := &scriptedReviewer{results: [][]Diagnostic{{
		diag("lint", "a.txt", "first"),
		diag("style", "d.txt", "second"),
		{Rule: "lint", Path: "d.txt", Message: "already fixed", Fixed: true},
	}, {
		diag("novel", "d.txt", "discovered late"),
	}}}

	gate := SubmitGate[*struct{}](reviewer)
	ctx := clonemanager.WithWorktree(t.Context(), gateWorktree(t))

	// First evaluation: every unfixed diagnostic blocks; Fixed is ignored.
	findings, err := gate(ctx, nil, "reasoning")
	if err != nil {
		t.Fatalf("first evaluation: got = %v, want = nil", err)
	}
	want := []string{"lint:a.txt:1", "style:d.txt:1"}
	got := make([]string, 0, len(findings))
	for _, f := range findings {
		got = append(got, f.Identifier)
	}
	if !slices.Equal(got, want) {
		t.Errorf("identifiers: got = %v, want = %v", got, want)
	}

	// Evaluations are independent: whatever the reviewer reports now blocks
	// now, and nothing rides along as prior.
	findings, err = gate(ctx, nil, "reasoning")
	if err != nil {
		t.Fatalf("second evaluation: got = %v, want = nil", err)
	}
	if len(findings) != 1 || findings[0].Identifier != "novel:d.txt:1" {
		t.Errorf("second evaluation findings: got = %v, want one with identifier %q", findings, "novel:d.txt:1")
	}
	for i, call := range reviewer.calls {
		if len(call.prior) != 0 {
			t.Errorf("calls[%d].prior: got = %v, want = none", i, call.prior)
		}
	}

	// A reviewer that runs dry accepts.
	findings, err = gate(ctx, nil, "reasoning")
	if err != nil {
		t.Fatalf("third evaluation: got = %v, want = nil", err)
	}
	if len(findings) != 0 {
		t.Errorf("third evaluation findings: got = %v, want = none", findings)
	}
}

func TestSubmitGateReviewerErrorFailsLoud(t *testing.T) {
	t.Parallel()

	boom := errors.New("reviewer exploded")
	reviewer := &scriptedReviewer{err: boom}
	ctx := clonemanager.WithWorktree(t.Context(), gateWorktree(t))

	_, err := SubmitGate[*struct{}](reviewer)(ctx, nil, "reasoning")
	if !errors.Is(err, boom) {
		t.Fatalf("SubmitGate error: got = %v, want = %v", err, boom)
	}
	// The error aborts the whole run, so it must name the failing reviewer's
	// type — an index alone is not diagnosable from an incident.
	if want := "*metapathreconciler.scriptedReviewer"; !strings.Contains(err.Error(), want) {
		t.Errorf("error text: got = %q, want it to contain %q", err, want)
	}
}

func TestChangedPaths(t *testing.T) {
	t.Parallel()

	paths, err := changedPaths(gateWorktree(t))
	if err != nil {
		t.Fatalf("changedPaths: got = %v, want = nil", err)
	}
	if want := []string{"a.txt", "d.txt", "e.txt"}; !slices.Equal(paths, want) {
		t.Errorf("paths: got = %v, want = %v", paths, want)
	}
}
