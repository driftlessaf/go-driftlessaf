/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/runbudget"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// testBudget is a run budget whose deadlines are far enough out that no test
// here can reach them; the tests exercise detachment, not expiry.
var testBudget = runbudget.Budget{Total: time.Hour, Reserve: 8 * time.Minute}

// leaseTestRepo initializes a remote, points the manager at it, and leases the
// default branch, returning the lease and the remote's path.
func leaseTestRepo(t *testing.T, ctx context.Context, opts ...Option) (*Lease, string) {
	t.Helper()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	repoDir, _ := initTestRepo(t)
	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Ref:   "master",
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}
	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	return lease, repoDir
}

// remoteBranchHash returns the hash the remote branch points at, or the zero
// hash when the branch is absent.
func remoteBranchHash(t *testing.T, repoDir, branch string) plumbing.Hash {
	t.Helper()
	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	ref, err := origin.Reference(plumbing.NewBranchReferenceName(branch), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash
	}
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	return ref.Hash()
}

// TestMakeAndPushChangesPublishesAfterRequestEnds proves that, under a run
// budget, a request that ends after the update function returns cannot stop
// the commit and push: publication runs detached from the request.
func TestMakeAndPushChangesPublishesAfterRequestEnds(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesPublishesAfterRequestEnds)
}

func testMakeAndPushChangesPublishesAfterRequestEnds(t *testing.T, opts ...Option) {
	request, endRequest := context.WithCancel(t.Context())
	defer endRequest()
	ctx := runbudget.Start(request, testBudget, time.Now())

	lease, repoDir := leaseTestRepo(t, ctx, opts...)
	defer func() {
		if err := lease.Return(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("Return: %v", err)
		}
	}()

	const branch = "clonemanager/publication-after-cancel"
	err := lease.MakeAndPushChanges(ctx, branch, func(_ context.Context, wt *git.Worktree) (string, error) {
		writeTestFile(t, wt.Filesystem.Root(), filepath.Join("packages", "bar.yaml"), "name: bar", 0o644)
		// The platform ends the request once the agent has returned.
		endRequest()
		return "add bar", nil
	})
	if err != nil {
		t.Fatalf("MakeAndPushChanges after the request ended: got err = %v, want nil", err)
	}
	if got := remoteBranchHash(t, repoDir, branch); got.IsZero() {
		t.Errorf("remote branch %q: absent after push, want present", branch)
	}
}

// TestMakeAndPushChangesWithoutBudgetHonorsCancel is the negative case: without
// a run budget, publication stays under the request context, so a request that
// ends after the update function returns discards the commit. The push runs
// under that context, so the cancellation surfaces there deterministically.
func TestMakeAndPushChangesWithoutBudgetHonorsCancel(t *testing.T) {
	request, endRequest := context.WithCancel(t.Context())
	defer endRequest()

	lease, repoDir := leaseTestRepo(t, request, WithGitCLI())
	defer func() {
		if err := lease.Return(context.WithoutCancel(request)); err != nil {
			t.Errorf("Return: %v", err)
		}
	}()

	const branch = "clonemanager/no-budget-cancel"
	err := lease.MakeAndPushChanges(request, branch, func(_ context.Context, wt *git.Worktree) (string, error) {
		writeTestFile(t, wt.Filesystem.Root(), filepath.Join("packages", "bar.yaml"), "name: bar", 0o644)
		endRequest()
		return "add bar", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MakeAndPushChanges without a budget after the request ended: got err = %v, want %v", err, context.Canceled)
	}
	if got := remoteBranchHash(t, repoDir, branch); !got.IsZero() {
		t.Errorf("remote branch %q: present at %s after a canceled publication, want absent", branch, got)
	}
}
