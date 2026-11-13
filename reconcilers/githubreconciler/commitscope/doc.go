/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package commitscope decides which working-tree changes a code-writing bot
// commits, so gates and finalizers that run against a checkout cannot slip
// artifacts into a signed commit.
//
// A bot edits a checkout through a managed callback layer, runs deterministic
// gates (build, lint, test, format), then commits. Those gates litter the tree
// with provider binaries, rewritten lock files, caches, compiled test binaries,
// coverage output, and reports. Staging the whole tree would commit all of it.
//
// The guard is two layers:
//
//  1. Intent staging. A [Scope] records the repo-relative paths the edit tools
//     wrote, moved, copied, or deleted during a run. The commit set starts from
//     those paths plus modifications to files already tracked at the base
//     revision (the finalizers rewrite tracked source in place). Everything else
//     that differs from the base tree is left uncommitted and logged.
//
//  2. Denylist. Regardless of who touched a file, [Denylist] refuses binary
//     content, a set of artifact path patterns, a symlink replaced by a regular
//     file, a Terraform lock file (unless the run declares a lock-file update),
//     and files above a size ceiling. A denied path is dropped even when an edit
//     tool wrote it; a run whose only changes are denied produces no commit.
//
// Limitation of intent staging: the second half of the commit set, "modifications
// to files tracked at the base," cannot tell a finalizer that deliberately
// rewrites tracked source from a read-only gate that rewrites a tracked file as a
// side effect (a formatter run in check mode that still writes the file, say).
// Both look like a modification to a tracked path, so intent staging keeps both.
// A gate whose incidental writes must stay out of the commit has to run inside a
// [Snapshot] and [Snapshot.Restore] pair: take the snapshot before the gate, call
// Restore after, and Restore reverts the gate's writes to tracked files (and
// removes the untracked files it created) while preserving edits made before the
// gate ran. Restore, not intent staging, is what closes this gap.
//
// [Plan] is the pure decision over a slice of [Change]. [Stage] gathers changes
// from a git worktree, applies [Plan], and stages the kept paths in the index.
// [Snapshot] and its Restore method give a read-only gate a way to record the
// tree before it runs and revert what it leaves behind; [Guarded] wraps a gate
// in that record-and-revert, resolving the base tree with [WorktreeBaseTree].
//
// Stage fails closed: any error reading worktree status aborts without staging,
// so a commit built on its output includes nothing rather than everything.
package commitscope
