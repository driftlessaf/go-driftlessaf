/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metapathreconciler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// snapshotFile is one path's recorded state: a regular file's content and
// mode, a symbolic link's target, or that the path was absent from the
// checkout.
type snapshotFile struct {
	data    []byte
	mode    fs.FileMode
	link    string
	symlink bool
	deleted bool
}

// checkoutSnapshot records a worktree's uncommitted changes — every path that
// differs from HEAD, with its content — and the commit HEAD named at the time,
// so the checkout can be returned to exactly that state after an agent run
// whose edits must not remain.
type checkoutSnapshot struct {
	files map[string]snapshotFile
	// commit is the HEAD commit when the snapshot was taken. restore resets to
	// it by hash rather than to whatever HEAD names afterward, so a pass that
	// rewrote the repository's references or index cannot redirect the reset.
	commit plumbing.Hash
}

// snapshotCheckout captures the worktree's uncommitted changes. A nil worktree
// yields a nil snapshot, which restore refuses, so a caller without a checkout
// keeps failing hard instead of pretending to revert.
func snapshotCheckout(wt *gogit.Worktree) (*checkoutSnapshot, error) {
	if wt == nil {
		return nil, nil
	}
	head, err := headCommit(wt)
	if err != nil {
		return nil, err
	}
	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("worktree status: %w", err)
	}
	root, err := openCheckout(wt)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snap := &checkoutSnapshot{files: make(map[string]snapshotFile, len(status)), commit: head}
	for path, st := range status {
		if st.Worktree == gogit.Unmodified && st.Staging == gogit.Unmodified {
			continue
		}
		f, err := recordFile(root, filepath.FromSlash(path))
		if err != nil {
			return nil, err
		}
		snap.files[path] = f
	}
	return snap, nil
}

// headCommit resolves the commit HEAD names in the worktree's repository at
// this moment. The worktree does not expose its repository, so it is opened
// from the checkout root.
func headCommit(wt *gogit.Worktree) (plumbing.Hash, error) {
	repo, err := gogit.PlainOpen(wt.Filesystem.Root())
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("open repository: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve HEAD: %w", err)
	}
	return head.Hash(), nil
}

// openCheckout opens the worktree root for file operations that cannot follow
// a symbolic link out of the checkout. An agent pass can leave a symlink at any
// path under the checkout, so every read and write of the snapshot goes
// through the root rather than the worktree's own filesystem.
func openCheckout(wt *gogit.Worktree) (*os.Root, error) {
	root, err := os.OpenRoot(wt.Filesystem.Root())
	if err != nil {
		return nil, fmt.Errorf("open checkout root: %w", err)
	}
	return root, nil
}

// recordFile captures one path without following a symbolic link: a link is
// recorded by its target, a regular file by its content and mode, and a
// missing path as deleted.
func recordFile(root *os.Root, path string) (snapshotFile, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshotFile{deleted: true}, nil
	}
	if err != nil {
		return snapshotFile{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(path)
		if err != nil {
			return snapshotFile{}, fmt.Errorf("readlink %s: %w", path, err)
		}
		return snapshotFile{symlink: true, link: target}, nil
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return snapshotFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	return snapshotFile{data: data, mode: info.Mode()}, nil
}

// restore returns the worktree to the snapshot. Symbolic links at changed
// paths are removed first, so the hard reset that follows cannot write a
// tracked file through a link the failed pass planted; the reset then takes
// every tracked file (and the index) back to the snapshot's commit, named by
// hash rather than by the current HEAD, paths the snapshot did not record are
// removed, and each recorded path is rewritten, re-linked, or deleted to
// match. The commit path stages the whole checkout before it
// commits, so the reset index costs nothing. An error is returned rather than
// a checkout left half-restored.
func (s *checkoutSnapshot) restore(wt *gogit.Worktree) error {
	if s == nil || wt == nil {
		return errors.New("no checkout snapshot to restore")
	}
	root, err := openCheckout(wt)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := removeChangedSymlinks(wt, root); err != nil {
		return err
	}
	if err := wt.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: s.commit}); err != nil {
		return fmt.Errorf("reset worktree to %s: %w", s.commit, err)
	}
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("worktree status after reset: %w", err)
	}
	// What the reset left behind and the snapshot never saw is the failed
	// pass's own; it goes before the recorded paths are written so no write
	// can land under it.
	for path, st := range status {
		if st.Worktree == gogit.Unmodified && st.Staging == gogit.Unmodified {
			continue
		}
		if _, recorded := s.files[path]; recorded {
			continue
		}
		if err := removeFile(root, filepath.FromSlash(path)); err != nil {
			return err
		}
	}
	for path, f := range s.files {
		if err := restoreFile(root, filepath.FromSlash(path), f); err != nil {
			return err
		}
	}
	return nil
}

// removeChangedSymlinks deletes every symbolic link among the worktree's
// changed paths. Recorded links are recreated by restoreFile afterward; the
// rest belong to the failed pass. Removing them before the reset keeps go-git
// from opening a tracked path through a link when it rewrites HEAD's content.
func removeChangedSymlinks(wt *gogit.Worktree, root *os.Root) error {
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("worktree status before reset: %w", err)
	}
	for path, st := range status {
		if st.Worktree == gogit.Unmodified && st.Staging == gogit.Unmodified {
			continue
		}
		name := filepath.FromSlash(path)
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if err := removeFile(root, name); err != nil {
			return err
		}
	}
	return nil
}

// restoreFile puts one recorded path back: removed, re-linked, or rewritten
// with its content and mode. Whatever now occupies the path is removed first,
// so a link is recreated as a link and a file is never written through one.
func restoreFile(root *os.Root, path string, f snapshotFile) error {
	if err := removeFile(root, path); err != nil {
		return err
	}
	if f.deleted {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", path, err)
		}
	}
	if f.symlink {
		if err := root.Symlink(f.link, path); err != nil {
			return fmt.Errorf("symlink %s: %w", path, err)
		}
		return nil
	}
	if err := root.WriteFile(path, f.data, f.mode.Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// removeFile deletes path from the checkout, treating an already-absent path
// as removed. A symbolic link is removed as a link, never followed.
func removeFile(root *os.Root, path string) error {
	if err := root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
