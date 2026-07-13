/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"testing"

	git "github.com/go-git/go-git/v5"
)

func TestWorktreeFromContext(t *testing.T) {
	t.Parallel()

	if wt, ok := WorktreeFromContext(t.Context()); ok || wt != nil {
		t.Errorf("WorktreeFromContext on bare context: got = (%v, %v), want = (nil, false)", wt, ok)
	}

	// A typed-nil worktree must not report present: consumers branch on the
	// boolean and would otherwise panic on first use of the pointer.
	if wt, ok := WorktreeFromContext(WithWorktree(t.Context(), nil)); ok || wt != nil {
		t.Errorf("WorktreeFromContext with nil worktree: got = (%v, %v), want = (nil, false)", wt, ok)
	}

	dir, _ := initTestRepo(t)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	want, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	got, ok := WorktreeFromContext(WithWorktree(t.Context(), want))
	if !ok || got != want {
		t.Errorf("WorktreeFromContext round-trip: got = (%p, %v), want = (%p, true)", got, ok, want)
	}
}
