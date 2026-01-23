/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/githubreconciler"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/oauth2"
)

func TestLeaseLifecycle(t *testing.T) {
	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)

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

	if got := lease.SHA(); got != headHash {
		t.Fatalf("SHA mismatch, got %s want %s", got, headHash)
	}

	if !lease.PathExists() {
		t.Fatalf("expected path to exist")
	}

	workingDir := lease.WorkingTree()
	if workingDir == repoDir {
		t.Fatalf("expected working dir to differ from remote")
	}

	scratch := filepath.Join(workingDir, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("temporary"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := lease.Return(ctx); err != nil {
		t.Fatalf("returning lease: %v", err)
	}

	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease reuse: %v", err)
	}

	if lease2.WorkingTree() != workingDir {
		t.Fatalf("expected clone to be reused")
	}

	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected scratch file cleaned, got err=%v", err)
	}

	missing := *res
	missing.Path = "packages/missing.yaml"
	leaseMissing, err := mgr.Lease(ctx, &missing)
	if err != nil {
		t.Fatalf("Lease missing path: %v", err)
	}
	if leaseMissing.PathExists() {
		t.Fatalf("expected missing path to report false")
	}
	if err := leaseMissing.Return(ctx); err != nil {
		t.Fatalf("returning missing lease: %v", err)
	}

	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("returning lease2: %v", err)
	}
}

func TestMakeAndPushChanges(t *testing.T) {
	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil)
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

	branchName := "clonemanager/test-branch"

	if err := lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		relPath := filepath.ToSlash(filepath.Join("packages", "bar.yaml"))
		absPath := filepath.Join(wt.Filesystem.Root(), relPath)
		if err := os.WriteFile(absPath, []byte("name: bar"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}

		if _, err := wt.Add(relPath); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}

		return "add bar", nil
	}); err != nil {
		t.Fatalf("MakeAndPushChanges: %v", err)
	}

	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return: %v", err)
	}

	originRepo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}

	if _, err := originRepo.Reference(plumbing.NewBranchReferenceName(branchName), true); err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
}

func initTestRepo(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	pkgDir := filepath.Join(dir, "packages")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	file := filepath.Join(pkgDir, "foo.yaml")
	if err := os.WriteFile(file, []byte("name: foo"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := wt.Add("packages/foo.yaml"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	hash, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))); err != nil {
		t.Fatalf("SetReference: %v", err)
	}

	return dir, hash.String()
}

// TestFIFOPoolBehavior verifies that the clone pool prevents churning by
// ensuring recently returned clones are not immediately reused. Clones are
// released to the back of the pool and acquired from the front, so the oldest
// returned clone is acquired next. This allows problematic clones to age out
// at the back of the pool rather than being reused repeatedly.
func TestFIFOPoolBehavior(t *testing.T) {
	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil)
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

	// Acquire three leases, creating three clones in the pool.
	lease1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 2: %v", err)
	}
	lease3, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 3: %v", err)
	}

	// Record working directories to track clone identity.
	dir1 := lease1.WorkingTree()
	dir2 := lease2.WorkingTree()
	dir3 := lease3.WorkingTree()

	// Return clones in order: 1, 2, 3.
	// Pool state after returns: [1, 2, 3] (front to back).
	if err := lease1.Return(ctx); err != nil {
		t.Fatalf("Return lease1: %v", err)
	}
	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("Return lease2: %v", err)
	}
	if err := lease3.Return(ctx); err != nil {
		t.Fatalf("Return lease3: %v", err)
	}

	// With FIFO semantics (acquire from front, release to back):
	// - First acquire should get clone 1 (front of pool).
	// - Second acquire should get clone 2.
	// - Third acquire should get clone 3.
	reacquired1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Reacquire 1: %v", err)
	}
	reacquired2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Reacquire 2: %v", err)
	}
	reacquired3, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Reacquire 3: %v", err)
	}

	// Verify FILO order: most recently returned (3) should be last to be acquired.
	if got := reacquired1.WorkingTree(); got != dir1 {
		t.Errorf("First reacquire: got %s, want %s (clone 1)", got, dir1)
	}
	if got := reacquired2.WorkingTree(); got != dir2 {
		t.Errorf("Second reacquire: got %s, want %s (clone 2)", got, dir2)
	}
	if got := reacquired3.WorkingTree(); got != dir3 {
		t.Errorf("Third reacquire: got %s, want %s (clone 3)", got, dir3)
	}

	// Cleanup.
	_ = reacquired1.Return(ctx)
	_ = reacquired2.Return(ctx)
	_ = reacquired3.Return(ctx)
}

type staticTokenSource string

func (s staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: string(s)}, nil
}
