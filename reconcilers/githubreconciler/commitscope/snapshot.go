/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// snapshotCaptureLimit bounds the content Snapshot captures per dirty tracked
// file. A file larger than this is reverted from the base tree instead of from a
// captured copy, so a snapshot cannot pin unbounded memory.
const snapshotCaptureLimit = 8 << 20 // 8 MiB

// Snapshot records a worktree's dirty state before a read-only gate runs, so the
// changes the gate leaves behind can be reverted. It captures the status of every
// changed path and the content of dirty tracked files, so an edit made before the
// gate ran survives a Restore while the gate's own writes do not.
type Snapshot struct {
	codes   map[string]gogit.FileStatus
	content map[string][]byte
}

// TakeSnapshot captures the worktree's current dirty state. It reads content only
// through a confined root.
func TakeSnapshot(wt *gogit.Worktree) (*Snapshot, error) {
	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("reading worktree status: %w", err)
	}
	root, err := os.OpenRoot(wt.Filesystem.Root())
	if err != nil {
		return nil, fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()

	snap := &Snapshot{
		codes:   make(map[string]gogit.FileStatus, len(status)),
		content: make(map[string][]byte),
	}
	for p, st := range status {
		np, ok := confined(p)
		if !ok {
			continue
		}
		snap.codes[np] = *st
		if st.Worktree == gogit.Untracked || st.Worktree == gogit.Deleted {
			continue
		}
		if b, ok := readCapped(root, np, snapshotCaptureLimit); ok {
			snap.content[np] = b
		}
	}
	return snap, nil
}

// Restore reverts changes made after the snapshot: it removes untracked files
// that appeared, restores dirty tracked files that changed to their snapshot
// content, and restores tracked files that were clean at snapshot to their base
// content. It leaves changes that predate the snapshot untouched. baseTree
// supplies the content and mode for a file that was clean at snapshot.
//
// Restore attempts every path and returns an error naming the count it could not
// revert. A caller that wraps a read-only gate must treat that error as fatal and
// abort rather than commit the residue the gate left, since the tracked-file
// proxy in Stage would otherwise stage a gate's rewrite of a tracked file.
func (s *Snapshot) Restore(ctx context.Context, wt *gogit.Worktree, baseTree *object.Tree) error {
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("reading worktree status: %w", err)
	}
	root, err := os.OpenRoot(wt.Filesystem.Root())
	if err != nil {
		return fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()

	var removed, reverted, failed int
	for p, st := range status {
		np, ok := confined(p)
		if !ok {
			continue
		}
		prev, existed := s.codes[np]
		if existed && *st == prev && !s.contentDrifted(root, np) {
			continue
		}

		if st.Worktree == gogit.Untracked {
			if existed && prev.Worktree == gogit.Untracked {
				continue // untracked before the gate ran; leave it
			}
			if err := removePath(root, np); err != nil {
				clog.WarnContextf(ctx, "read-only guard: could not remove %q: %v", np, err)
				failed++
				continue
			}
			removed++
			continue
		}

		if err := s.revert(root, baseTree, np); err != nil {
			clog.WarnContextf(ctx, "read-only guard: could not revert %q: %v", np, err)
			failed++
			continue
		}
		reverted++
	}

	clog.InfoContextf(ctx, "read-only guard: reverted %d modification(s), removed %d new path(s)", reverted, removed)
	if failed > 0 {
		// Fail closed: report so a caller wrapping a read-only gate can abort
		// instead of committing residue the guard could not revert.
		return fmt.Errorf("read-only guard: %d path(s) could not be reverted", failed)
	}
	return nil
}

// contentDrifted reports whether a captured dirty tracked file differs from its
// captured content now (the gate rewrote a file the agent had already edited).
func (s *Snapshot) contentDrifted(root *os.Root, np string) bool {
	captured, ok := s.content[np]
	if !ok {
		return false
	}
	cur, ok := readCapped(root, np, snapshotCaptureLimit)
	if !ok {
		return true
	}
	return !bytes.Equal(cur, captured)
}

// revert restores a tracked path to its snapshot content when one was captured,
// otherwise to its base content (it was clean when the snapshot was taken).
func (s *Snapshot) revert(root *os.Root, baseTree *object.Tree, np string) error {
	if captured, ok := s.content[np]; ok {
		return writeFile(root, np, captured, 0o644)
	}
	return revertToBase(root, baseTree, np)
}

// revertToBase rewrites a path from the base tree, recreating a symlink as a
// symlink and removing a path the base does not contain.
func revertToBase(root *os.Root, baseTree *object.Tree, np string) error {
	if baseTree == nil {
		return removePath(root, np)
	}
	entry, err := baseTree.FindEntry(np)
	if err != nil {
		return removePath(root, np)
	}
	file, err := baseTree.File(np)
	if err != nil {
		return fmt.Errorf("reading base blob: %w", err)
	}
	contents, err := file.Contents()
	if err != nil {
		return fmt.Errorf("reading base content: %w", err)
	}
	if entry.Mode == filemode.Symlink {
		if err := removePath(root, np); err != nil {
			return err
		}
		return root.Symlink(contents, np)
	}
	perm := os.FileMode(0o644)
	if entry.Mode == filemode.Executable {
		perm = 0o755
	}
	return writeFile(root, np, []byte(contents), perm)
}

// readCapped reads a regular file through root, up to limit bytes. It reports
// false for a non-regular file (symlink, directory) or a file larger than limit.
func readCapped(root *os.Root, np string, limit int64) ([]byte, bool) {
	fi, err := root.Lstat(np)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := root.Open(np)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, false
	}
	return b, true
}

// writeFile writes data through root, creating the parent directory if the base
// tree has it but the worktree lost it.
func writeFile(root *os.Root, np string, data []byte, perm os.FileMode) error {
	if err := root.WriteFile(np, data, perm); err != nil {
		if dir := path.Dir(np); dir != "." {
			if mkErr := root.MkdirAll(dir, 0o755); mkErr != nil {
				return fmt.Errorf("creating parent %q: %w", dir, mkErr)
			}
			return root.WriteFile(np, data, perm)
		}
		return err
	}
	return nil
}

// removePath removes a path through root, treating an already-absent path as
// success.
func removePath(root *os.Root, np string) error {
	if err := root.RemoveAll(np); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
