/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/chainguard-dev/clog"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// maxLoggedDrops caps the per-run "left uncommitted" log lines, so a tool that
// litters thousands of paths is visible without flooding the log.
const maxLoggedDrops = 100

// Stage computes the commit set for a worktree and stages exactly those paths in
// the git index. It reads the worktree status, classifies each change against the
// scope (the intended paths) and the denylist, stages the kept paths (additions,
// modifications, and intended deletions), logs the dropped paths at Info with a
// bounded count, and returns what it kept and dropped.
//
// Stage fails closed: any error reading worktree status aborts without staging,
// and a path it cannot classify is dropped rather than staged, so a commit built
// on its output includes nothing rather than everything.
//
// baseTree is the tree the branch was created from; it supplies the base mode
// and base content that distinguish a symlink replaced by a file and a tracked
// binary from new binary content. A nil baseTree treats every path as new.
func Stage(ctx context.Context, wt *gogit.Worktree, baseTree *object.Tree, scope *Scope, d Denylist) (Result, error) {
	status, err := wt.Status()
	if err != nil {
		return Result{}, fmt.Errorf("reading worktree status: %w", err)
	}

	root, err := os.OpenRoot(wt.Filesystem.Root())
	if err != nil {
		return Result{}, fmt.Errorf("opening worktree root: %w", err)
	}
	defer root.Close()

	d.AllowLockUpdate = d.AllowLockUpdate || scope.LockUpdateDeclared()

	changes := make([]Change, 0, len(status))
	var inspectDrops []Dropped
	for p, st := range status {
		if st.Staging == gogit.Unmodified && st.Worktree == gogit.Unmodified {
			continue
		}
		np, ok := confined(p)
		if !ok {
			// Fail closed: a path that cleans to the worktree root or escapes it
			// is never staged. Staging "." would add the entire tree, defeating
			// the guard.
			inspectDrops = append(inspectDrops, Dropped{Path: p, Reason: "path is not confined to the worktree"})
			clog.WarnContextf(ctx, "commit guard: refusing unconfined path %q", p)
			continue
		}
		c, err := classify(root, baseTree, scope, np, st)
		if err != nil {
			// Fail closed: a path the guard cannot classify is never staged.
			inspectDrops = append(inspectDrops, Dropped{Path: np, Reason: "could not classify change"})
			clog.WarnContextf(ctx, "commit guard: could not classify %q: %v", np, err)
			continue
		}
		changes = append(changes, c)
	}

	res := Plan(changes, d)
	res.Dropped = append(res.Dropped, inspectDrops...)

	// Stage the kept paths. Stage runs once, single-threaded, before the commit,
	// so go-git's non-atomic index writes do not race.
	for _, c := range res.Staged {
		if err := stageOne(wt, c); err != nil {
			return Result{}, fmt.Errorf("staging %q: %w", c.Path, err)
		}
	}

	logDropped(ctx, res.Dropped)
	return res, nil
}

// confined returns the repo-relative, slash-cleaned form of a worktree status
// path together with whether it is safe to act on. A path that cleans to empty
// (the worktree root) or escapes the worktree is not confined; the caller drops
// it rather than staging it or reading through the confined root.
func confined(raw string) (string, bool) {
	np := normalizePath(raw)
	if np == "" || !fs.ValidPath(np) {
		return "", false
	}
	return np, true
}

// classify builds a Change from a git status entry and the filesystem. p is the
// cleaned, worktree-confined path the caller validated (see confined); classify
// reads content and mode only through the confined root, so a planted symlink
// cannot redirect a read outside the worktree.
func classify(root *os.Root, baseTree *object.Tree, scope *Scope, p string, st *gogit.FileStatus) (Change, error) {
	typ := classifyType(st)
	tracked := typ != Added
	c := Change{
		Path:          p,
		Type:          typ,
		TrackedAtBase: tracked,
		// An edit tool recorded the path, or a finalizer rewrote a tracked file
		// in place (a tracked modification the callback layer did not record).
		Intended: scope.Contains(p) || (tracked && typ == Modified),
	}

	if typ == Deleted {
		return c, nil
	}

	fi, err := root.Lstat(p)
	if err != nil {
		return Change{}, fmt.Errorf("lstat: %w", err)
	}
	c.CurrentIsSymlink = fi.Mode()&os.ModeSymlink != 0
	c.Size = fi.Size()

	if tracked && baseTree != nil {
		if entry, err := baseTree.FindEntry(p); err == nil {
			c.BaseIsSymlink = entry.Mode == filemode.Symlink
		}
	}

	if !c.CurrentIsSymlink && fi.Mode().IsRegular() {
		bin, err := sniffFileBinary(root, p)
		if err != nil {
			return Change{}, fmt.Errorf("sniff: %w", err)
		}
		c.IsBinary = bin
		if c.IsBinary && tracked && baseTree != nil {
			c.BaseIsBinary = baseBlobBinary(baseTree, p)
		}
	}

	return c, nil
}

// classifyType maps go-git status codes to a ChangeType.
func classifyType(st *gogit.FileStatus) ChangeType {
	switch {
	case st.Worktree == gogit.Deleted || st.Staging == gogit.Deleted:
		return Deleted
	case st.Worktree == gogit.Untracked:
		return Added
	case st.Staging == gogit.Added && st.Worktree == gogit.Unmodified:
		return Added
	default:
		return Modified
	}
}

// stageOne stages a single kept change: a deletion is removed from the index, an
// addition or modification is added.
func stageOne(wt *gogit.Worktree, c Change) error {
	if c.Type == Deleted {
		_, err := wt.Remove(c.Path)
		return err
	}
	_, err := wt.Add(c.Path)
	return err
}

// sniffFileBinary sniffs the current content of a worktree file through the
// confined root, reading at most the sniff window.
func sniffFileBinary(root *os.Root, p string) (bool, error) {
	f, err := root.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return sniffBinary(io.LimitReader(f, sniffLimit))
}

// baseBlobBinary reports whether the base content of a tracked path sniffs as
// binary. A missing or unreadable base blob reports false.
func baseBlobBinary(tree *object.Tree, p string) bool {
	f, err := tree.File(p)
	if err != nil {
		return false
	}
	r, err := f.Reader()
	if err != nil {
		return false
	}
	defer r.Close()
	bin, err := sniffBinary(io.LimitReader(r, sniffLimit))
	if err != nil {
		return false
	}
	return bin
}

// logDropped emits one "left uncommitted" line per dropped path at Info, up to a
// bound, then a single summary for the remainder. Paths are quoted so a crafted
// name cannot forge log lines.
func logDropped(ctx context.Context, dropped []Dropped) {
	for i, dr := range dropped {
		if i >= maxLoggedDrops {
			clog.InfoContextf(ctx, "commit guard: left uncommitted %d further path(s)", len(dropped)-maxLoggedDrops)
			return
		}
		clog.InfoContextf(ctx, "left uncommitted: %q (%s)", dr.Path, dr.Reason)
	}
}
