/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"context"
	"errors"
	"fmt"

	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Guarded runs a read-only gate under a snapshot so anything the gate leaves in
// the worktree is reverted before the commit. It captures the worktree's dirty
// state with [TakeSnapshot], runs fn, then reverts the gate's writes with
// [Snapshot.Restore]: untracked files the gate created are removed, tracked files
// it rewrote are returned to their pre-gate content, and edits that predate the
// gate are preserved. baseTree supplies the content for a tracked file that was
// clean when the snapshot was taken; pass the worktree's current HEAD tree, which
// [WorktreeBaseTree] resolves.
//
// A snapshot that cannot be taken is logged and fn runs unguarded, because the
// commit stage's denylist still backstops the untracked artifacts a gate creates.
// A restore that fails, by contrast, is fatal: it means the gate rewrote a tracked
// file the guard could not revert, which the commit stage treats as an intended
// modification and would stage, so Guarded returns that error (joined with fn's
// own error, when set) for the caller to abort the run on before the commit. A nil
// worktree runs fn directly.
func Guarded(ctx context.Context, wt *gogit.Worktree, baseTree *object.Tree, fn func() error) error {
	if fn == nil {
		return nil
	}
	if wt == nil {
		return fn()
	}
	snap, err := TakeSnapshot(wt)
	if err != nil {
		clog.WarnContextf(ctx, "read-only guard: snapshot failed, running unguarded: %v", err)
		return fn()
	}
	fnErr := fn()
	if rErr := snap.Restore(ctx, wt, baseTree); rErr != nil {
		// Fatal: the gate rewrote a tracked file the guard could not revert, and the
		// commit stage would stage that rewrite as an intended modification. Surface
		// it (with fn's own error, when set) so the caller aborts before the commit.
		return errors.Join(fnErr, rErr)
	}
	return fnErr
}

// WorktreeBaseTree returns the tree of the worktree's current HEAD commit, the
// base a [Guarded] gate reverts a clean tracked file to. It opens the repository
// at the worktree root read-only, so a caller that holds only a worktree can
// resolve the base tree without a separate repository handle.
func WorktreeBaseTree(wt *gogit.Worktree) (*object.Tree, error) {
	if wt == nil || wt.Filesystem == nil {
		return nil, errors.New("nil worktree")
	}
	repo, err := gogit.PlainOpen(wt.Filesystem.Root())
	if err != nil {
		return nil, fmt.Errorf("opening repository: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolving HEAD: %w", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("reading HEAD commit: %w", err)
	}
	return commit.Tree()
}
