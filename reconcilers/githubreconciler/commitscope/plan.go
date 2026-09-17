/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

// ChangeType classifies how a working-tree path differs from the base revision.
type ChangeType int

const (
	// Added is a path untracked at the base revision.
	Added ChangeType = iota
	// Modified is a tracked path whose content or mode changed.
	Modified
	// Deleted is a tracked path removed from the worktree.
	Deleted
)

// String renders a ChangeType for logs and test failures.
func (t ChangeType) String() string {
	switch t {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	default:
		return "unknown"
	}
}

// Change is one working-tree path the guard classifies. Stage builds these from
// git status and the filesystem; Plan and [Denylist.Denied] read them.
type Change struct {
	// Path is the repo-relative, slash-separated path.
	Path string
	// Type is how the path differs from the base revision.
	Type ChangeType
	// Intended reports that an edit tool or a finalizer produced this change,
	// as opposed to a read-only gate or an unrelated tool.
	Intended bool
	// TrackedAtBase reports that the path existed at the base revision.
	TrackedAtBase bool
	// CurrentIsSymlink reports that the path is a symbolic link now.
	CurrentIsSymlink bool
	// BaseIsSymlink reports that the path was a symbolic link at the base
	// revision.
	BaseIsSymlink bool
	// IsBinary reports that the current content sniffs as binary.
	IsBinary bool
	// BaseIsBinary reports that the base content sniffed as binary.
	BaseIsBinary bool
	// Size is the current file size in bytes (0 for a deletion).
	Size int64
}

// Dropped is a change the guard kept out of the commit, with a short reason.
type Dropped struct {
	// Path is the repo-relative path that was dropped.
	Path string
	// Reason explains why, for the "left uncommitted" log line.
	Reason string
}

// Result is the outcome of Plan: the changes to stage and the changes dropped.
type Result struct {
	// Staged are the changes the guard commits.
	Staged []Change
	// Dropped are the changes the guard leaves uncommitted, with reasons.
	Dropped []Dropped
}

// Plan partitions changes into the commit set and the dropped set. A change is
// staged when it is intended and no denylist rule matches it; otherwise it is
// dropped with a reason. Plan is pure: it reads only its arguments.
func Plan(changes []Change, d Denylist) Result {
	res := Result{Staged: make([]Change, 0, len(changes))}
	for _, c := range changes {
		if !c.Intended {
			res.Dropped = append(res.Dropped, Dropped{Path: c.Path, Reason: "not produced by an edit tool or finalizer"})
			continue
		}
		if deny, reason := d.Denied(c); deny {
			res.Dropped = append(res.Dropped, Dropped{Path: c.Path, Reason: reason})
			continue
		}
		res.Staged = append(res.Staged, c)
	}
	return res
}
