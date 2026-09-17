/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRestore(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{
		"keep.go":  "package keep\n",
		"prior.go": "package prior\n",
	})
	commitAll(t, wt)
	base := headTree(t, repo)

	// An agent edit made before the read-only gate runs.
	writeFileAt(t, root, "prior.go", "package prior // agent edit\n")

	snap, err := TakeSnapshot(wt)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// The read-only gate litters: a new untracked file and a rewrite of a file
	// that was clean when the snapshot was taken.
	writeFileAt(t, root, "coverage.out", "mode: set\n")
	writeFileAt(t, root, "keep.go", "package keep // clobbered by a gate\n")

	if err := snap.Restore(t.Context(), wt, base); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The gate's untracked output is gone.
	if _, err := os.Stat(filepath.Join(root, "coverage.out")); !os.IsNotExist(err) {
		t.Errorf("coverage.out survived restore: err = %v", err)
	}
	// The clean-at-snapshot file reverted to base content.
	if got := readFile(t, root, "keep.go"); got != "package keep\n" {
		t.Errorf("keep.go: got = %q, want base content", got)
	}
	// The agent edit made before the snapshot survives.
	if got := readFile(t, root, "prior.go"); got != "package prior // agent edit\n" {
		t.Errorf("prior.go: got = %q, want preserved agent edit", got)
	}
}

func TestSnapshotRestorePreservesDirtyFileGateRewrote(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{"edit.go": "package edit\n"})
	commitAll(t, wt)
	base := headTree(t, repo)

	// Agent edits edit.go before the snapshot.
	writeFileAt(t, root, "edit.go", "package edit // agent\n")
	snap, err := TakeSnapshot(wt)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	// A gate rewrites the already-edited file.
	writeFileAt(t, root, "edit.go", "package edit // gate clobber\n")

	if err := snap.Restore(t.Context(), wt, base); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Restore returns the file to its snapshot content, not base, preserving the
	// agent edit.
	if got := readFile(t, root, "edit.go"); got != "package edit // agent\n" {
		t.Errorf("edit.go: got = %q, want snapshot content", got)
	}
}

// TestSnapshotRestoreReportsUnrevertablePath proves Restore fails closed: when a
// path it must revert cannot be written (the gate replaced a tracked file with a
// directory), Restore returns an error so a caller can abort rather than commit
// the gate's residue.
func TestSnapshotRestoreReportsUnrevertablePath(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{"edit.go": "package edit\n"})
	commitAll(t, wt)
	base := headTree(t, repo)

	// Agent edits before the snapshot, so its content is captured.
	writeFileAt(t, root, "edit.go", "package edit // agent\n")
	snap, err := TakeSnapshot(wt)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// A gate replaces the tracked file with a directory: reverting the file back
	// cannot write through a directory, so the revert must fail.
	if err := os.Remove(filepath.Join(root, "edit.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "edit.go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileAt(t, root, filepath.Join("edit.go", "inner"), "gate output\n")

	if err := snap.Restore(t.Context(), wt, base); err == nil {
		t.Fatal("Restore returned nil, want an error for the unrevertable path")
	}
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	return string(b)
}
