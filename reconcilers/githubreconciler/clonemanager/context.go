/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"

	gogit "github.com/go-git/go-git/v5"
)

// worktreeKey is the context key under which the leased git worktree rides.
type worktreeKey struct{}

// WithWorktree returns a context carrying the leased git worktree.
// Lease.MakeAndPushChanges installs it on the context it passes to its update
// function, so code running under the update closure that cannot receive the
// worktree as a parameter — most notably result validators gating an agent's
// terminal submit tool (see metapathreconciler.SubmitGate) — can recover it
// with WorktreeFromContext.
func WithWorktree(ctx context.Context, wt *gogit.Worktree) context.Context {
	return context.WithValue(ctx, worktreeKey{}, wt)
}

// WorktreeFromContext returns the leased git worktree carried by the context,
// if any. The boolean reports whether one was present: a nil worktree — a
// typed-nil installed by a caller's zero value — reports false rather than
// handing consumers a pointer that panics on first use.
func WorktreeFromContext(ctx context.Context) (*gogit.Worktree, bool) {
	wt, ok := ctx.Value(worktreeKey{}).(*gogit.Worktree)
	if wt == nil {
		return nil, false
	}
	return wt, ok
}
