/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGuardedRevertsGateArtifacts(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{
		"keep.go":  "package keep\n",
		"prior.go": "package prior\n",
	})
	commitAll(t, wt)

	baseTree, err := WorktreeBaseTree(wt)
	if err != nil {
		t.Fatalf("WorktreeBaseTree: %v", err)
	}

	// An agent edit made before the read-only gate runs.
	writeFileAt(t, root, "prior.go", "package prior // agent edit\n")

	// The gate creates an untracked artifact and rewrites a file that was clean
	// when Guarded took its snapshot.
	err = Guarded(t.Context(), wt, baseTree, func() error {
		writeFileAt(t, root, "coverage.out", "mode: set\n")
		writeFileAt(t, root, "keep.go", "package keep // clobbered by a gate\n")
		return nil
	})
	if err != nil {
		t.Fatalf("Guarded: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "coverage.out")); !os.IsNotExist(err) {
		t.Errorf("coverage.out survived the guard: err = %v", err)
	}
	if got := readFile(t, root, "keep.go"); got != "package keep\n" {
		t.Errorf("keep.go: got = %q, want base content", got)
	}
	if got := readFile(t, root, "prior.go"); got != "package prior // agent edit\n" {
		t.Errorf("prior.go: got = %q, want preserved agent edit", got)
	}
	// A change never happened through the guard survives it: repo is unused
	// otherwise, so assert HEAD is untouched.
	if paths := committedPaths(t, repo); len(paths) != 2 {
		t.Errorf("HEAD tree changed unexpectedly: %v", paths)
	}
}

func TestGuardedPropagatesError(t *testing.T) {
	_, wt, root := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)
	baseTree, err := WorktreeBaseTree(wt)
	if err != nil {
		t.Fatalf("WorktreeBaseTree: %v", err)
	}

	gateErr := errors.New("gate failed")
	got := Guarded(t.Context(), wt, baseTree, func() error {
		writeFileAt(t, root, "coverage.out", "mode: set\n")
		return gateErr
	})
	if !errors.Is(got, gateErr) {
		t.Errorf("Guarded error = %v, want %v", got, gateErr)
	}
	// The gate's artifact is still reverted even though the gate failed.
	if _, err := os.Stat(filepath.Join(root, "coverage.out")); !os.IsNotExist(err) {
		t.Errorf("coverage.out survived the guard after a gate error: err = %v", err)
	}
}

func TestGuardedNilWorktreeRunsFn(t *testing.T) {
	ran := false
	if err := Guarded(t.Context(), nil, nil, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("Guarded: %v", err)
	}
	if !ran {
		t.Error("Guarded did not run fn for a nil worktree")
	}
	if err := Guarded(t.Context(), nil, nil, nil); err != nil {
		t.Errorf("Guarded with nil fn: %v", err)
	}
}

func TestWorktreeBaseTree(t *testing.T) {
	repo, wt, _ := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)

	tree, err := WorktreeBaseTree(wt)
	if err != nil {
		t.Fatalf("WorktreeBaseTree: %v", err)
	}
	if tree.Hash != headTree(t, repo).Hash {
		t.Errorf("WorktreeBaseTree hash = %v, want HEAD tree", tree.Hash)
	}

	if _, err := WorktreeBaseTree(nil); err == nil {
		t.Error("WorktreeBaseTree(nil) returned no error")
	}
}

func TestGuardedFatalWhenRestoreFails(t *testing.T) {
	_, wt, root := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)
	baseTree, err := WorktreeBaseTree(wt)
	if err != nil {
		t.Fatalf("WorktreeBaseTree: %v", err)
	}

	// The gate rewrites a clean tracked file, then makes it read-only so the guard
	// cannot revert it. A failed revert must surface as a fatal error, not a log,
	// because the commit stage would otherwise stage the gate's rewrite as an
	// intended tracked-file modification.
	keep := filepath.Join(root, "keep.go")
	gErr := Guarded(t.Context(), wt, baseTree, func() error {
		if werr := os.WriteFile(keep, []byte("package keep // gate rewrite\n"), 0o600); werr != nil {
			t.Fatalf("gate write: %v", werr)
		}
		if cerr := os.Chmod(keep, 0o400); cerr != nil {
			t.Fatalf("chmod: %v", cerr)
		}
		return nil
	})
	// Restore the mode so the temp dir cleanup can remove the file.
	_ = os.Chmod(keep, 0o600)

	if gErr == nil {
		t.Fatal("Guarded returned nil; want a fatal error when a tracked file cannot be reverted")
	}
}

func TestStageActiveScopeDropsArtifactWithNoEdits(t *testing.T) {
	_, wt, root := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)
	baseTree, err := WorktreeBaseTree(wt)
	if err != nil {
		t.Fatalf("WorktreeBaseTree: %v", err)
	}

	// A run made no edits (nothing recorded in the scope) but a gate left one
	// untracked artifact. With the scope marked active, as a guarded bot marks it,
	// Stage keeps the artifact out of the index, so the run produces no commit.
	writeFileAt(t, root, "coverage.out", "mode: set\n")
	scope := NewScope()
	scope.MarkActive()

	res, err := Stage(t.Context(), wt, baseTree, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(res.Staged) != 0 {
		t.Errorf("Stage staged %d path(s); want 0 for an artifact-only run", len(res.Staged))
	}
}
