/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initCheckout creates a repository whose first commit holds files and returns
// its worktree and root.
func initCheckout(t *testing.T, files map[string]string) (*gogit.Worktree, string) {
	t.Helper()
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for name, content := range files {
		writeCheckoutFile(t, root, name, content)
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0).UTC()}
	if _, err := wt.Commit("init", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return wt, root
}

func writeCheckoutFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

func removeCheckoutFile(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, name)); err != nil {
		t.Fatalf("Remove(%q): %v", name, err)
	}
}

// requireCheckout asserts the checkout holds exactly the given contents for
// the named files and none of the absent paths.
func requireCheckout(t *testing.T, root string, want map[string]string, absent []string) {
	t.Helper()
	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != content {
			t.Errorf("%s: got = %q, want = %q", name, got, content)
		}
	}
	for _, name := range absent {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s should be absent, stat err = %v", name, err)
		}
	}
}

func TestCheckoutSnapshotRestore(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n", "keep.txt": "keep\n", "gone.txt": "gone\n"})

	// The state a completed pass leaves: a tracked file edited, a file added,
	// a tracked file deleted.
	writeCheckoutFile(t, root, "base.txt", "edited\n")
	writeCheckoutFile(t, root, "sub/new.txt", "new\n")
	removeCheckoutFile(t, root, "gone.txt")
	snap, err := snapshotCheckout(wt)
	if err != nil {
		t.Fatalf("snapshotCheckout: %v", err)
	}

	// A later pass that must be undone touches every kind of path: it
	// clobbers a snapshot file, removes another, resurrects the deleted one,
	// edits an untouched tracked file, and adds one of its own.
	writeCheckoutFile(t, root, "base.txt", "clobbered\n")
	removeCheckoutFile(t, root, "sub/new.txt")
	writeCheckoutFile(t, root, "gone.txt", "back\n")
	writeCheckoutFile(t, root, "keep.txt", "touched\n")
	writeCheckoutFile(t, root, "junk.txt", "half-done\n")

	if err := snap.restore(wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	requireCheckout(t, root,
		map[string]string{"base.txt": "edited\n", "sub/new.txt": "new\n", "keep.txt": "keep\n"},
		[]string{"gone.txt", "junk.txt"},
	)
}

func TestCheckoutSnapshotRestoreToCleanCheckout(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	snap, err := snapshotCheckout(wt)
	if err != nil {
		t.Fatalf("snapshotCheckout: %v", err)
	}
	writeCheckoutFile(t, root, "base.txt", "edited\n")
	writeCheckoutFile(t, root, "added.txt", "added\n")

	if err := snap.restore(wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	requireCheckout(t, root, map[string]string{"base.txt": "base\n"}, []string{"added.txt"})
	status, err := wt.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.IsClean() {
		t.Errorf("checkout not clean after restoring an empty snapshot: %v", status)
	}
}

func TestCheckoutSnapshotWithoutWorktree(t *testing.T) {
	t.Parallel()
	snap, err := snapshotCheckout(nil)
	if err != nil || snap != nil {
		t.Fatalf("snapshotCheckout(nil) = %v, %v; want nil, nil", snap, err)
	}
	if err := snap.restore(nil); err == nil {
		t.Error("restore on a nil snapshot should fail rather than claim a revert")
	}
}

// TestCheckoutSnapshotRestoreNeverFollowsSymlinks plants, as a failed pass
// could, symbolic links out of the checkout at a tracked path, at a recorded
// directory, and at a fresh untracked path, and proves the restore removes
// them and rewrites the recorded paths in place rather than through them.
func TestCheckoutSnapshotRestoreNeverFollowsSymlinks(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}

	writeCheckoutFile(t, root, "base.txt", "edited\n")
	writeCheckoutFile(t, root, "sub/new.txt", "new\n")
	snap, err := snapshotCheckout(wt)
	if err != nil {
		t.Fatalf("snapshotCheckout: %v", err)
	}

	// The failed pass turns the tracked file into a link to the secret,
	// replaces the recorded directory with a link to the secret's directory,
	// and plants a third link at an untracked path.
	removeCheckoutFile(t, root, "base.txt")
	symlinkCheckoutPath(t, root, "base.txt", secret)
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("RemoveAll(sub): %v", err)
	}
	symlinkCheckoutPath(t, root, "sub", outside)
	symlinkCheckoutPath(t, root, "planted.txt", secret)

	if err := snap.restore(wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret\n" {
		t.Errorf("file outside the checkout: got = %q, %v; want untouched %q", got, err, "secret\n")
	}
	requireCheckout(t, root, map[string]string{"base.txt": "edited\n", "sub/new.txt": "new\n"}, []string{"planted.txt"})
	for _, name := range []string{"base.txt", "sub"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Errorf("Lstat(%s): %v", name, err)
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			t.Errorf("%s is still a symbolic link after restore", name)
		}
	}
}

// TestCheckoutSnapshotRecreatesRecordedSymlink proves a link that was part of
// the snapshotted state is recorded by its target, not read through, and comes
// back as a link, not as a file written through it.
func TestCheckoutSnapshotRecreatesRecordedSymlink(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}
	symlinkCheckoutPath(t, root, "link", secret)

	snap, err := snapshotCheckout(wt)
	if err != nil {
		t.Fatalf("snapshotCheckout: %v", err)
	}
	rec, ok := snap.files["link"]
	if !ok || !rec.symlink || rec.link != secret || rec.data != nil {
		t.Fatalf("recorded link = %+v, want a symlink to %q with no content", rec, secret)
	}

	// The failed pass replaces the link with a regular file.
	removeCheckoutFile(t, root, "link")
	writeCheckoutFile(t, root, "link", "overwrite\n")

	if err := snap.restore(wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret\n" {
		t.Errorf("link target: got = %q, %v; want untouched %q", got, err, "secret\n")
	}
	if target, err := os.Readlink(filepath.Join(root, "link")); err != nil || target != secret {
		t.Errorf("Readlink(link): got = %q, %v; want the recorded target %q", target, err, secret)
	}
}

func symlinkCheckoutPath(t *testing.T, root, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
		t.Fatalf("Symlink(%q -> %q): %v", name, target, err)
	}
}

// TestCheckoutSnapshotRestoreResetsToTheSnapshotCommit proves the restore
// targets the commit recorded at snapshot time, not whatever HEAD names when
// it runs: a pass that moved HEAD (here by committing) is unwound along with
// its files.
func TestCheckoutSnapshotRestoreResetsToTheSnapshotCommit(t *testing.T) {
	t.Parallel()
	wt, root := initCheckout(t, map[string]string{"base.txt": "base\n"})
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	before, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	writeCheckoutFile(t, root, "base.txt", "edited\n")
	snap, err := snapshotCheckout(wt)
	if err != nil {
		t.Fatalf("snapshotCheckout: %v", err)
	}
	if snap.commit != before.Hash() {
		t.Fatalf("snapshot commit = %s, want HEAD at snapshot time %s", snap.commit, before.Hash())
	}

	// The failed pass rewrites the repository state: it commits a file, which
	// moves HEAD past the snapshot.
	writeCheckoutFile(t, root, "junk.txt", "hijack\n")
	if _, err := wt.Add("junk.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sig := &object.Signature{Name: "pass", Email: "pass@example.com", When: time.Unix(1, 0).UTC()}
	if _, err := wt.Commit("hijack", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := snap.restore(wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, err := repo.Head()
	if err != nil {
		t.Fatalf("Head after restore: %v", err)
	}
	if after.Hash() != before.Hash() {
		t.Errorf("HEAD after restore = %s, want the snapshot commit %s", after.Hash(), before.Hash())
	}
	requireCheckout(t, root, map[string]string{"base.txt": "edited\n"}, []string{"junk.txt"})
}
