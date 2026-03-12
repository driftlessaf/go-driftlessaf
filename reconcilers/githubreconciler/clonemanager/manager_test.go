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

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
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

	// Commit a new file directly into the worktree, advancing HEAD beyond
	// the remote. This simulates a rogue commit must be undone.
	cloneRepo, err := git.PlainOpen(lease2.WorkingTree())
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	cloneWT, err := cloneRepo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	extraFile := filepath.Join("packages", "extra.yaml")
	if err := os.WriteFile(filepath.Join(lease2.WorkingTree(), extraFile), []byte("name: extra"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := cloneWT.Add(filepath.ToSlash(extraFile)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := cloneWT.Commit("add extra", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("returning lease2: %v", err)
	}

	// Reacquire and verify the clone is back to the original state.
	lease3, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after rogue commit: %v", err)
	}

	if got := lease3.SHA(); got != headHash {
		t.Errorf("SHA after reset: got %s, want %s", got, headHash)
	}

	if _, err := os.Stat(filepath.Join(lease3.WorkingTree(), extraFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected extra.yaml to be gone after reset, got err=%v", err)
	}

	if err := lease3.Return(ctx); err != nil {
		t.Fatalf("returning lease3: %v", err)
	}
}

func TestMakeAndPushChanges(t *testing.T) {
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

	branchName := "clonemanager/test-branch"
	barPath := filepath.ToSlash(filepath.Join("packages", "bar.yaml"))

	if err := lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		absPath := filepath.Join(wt.Filesystem.Root(), barPath)
		if err := os.WriteFile(absPath, []byte("name: bar"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}

		if _, err := wt.Add(barPath); err != nil {
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

	// Reacquire the lease and verify the clone was reset to the original
	// state: correct SHA, committed file removed.
	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after push: %v", err)
	}

	if got := lease2.SHA(); got != headHash {
		t.Errorf("SHA after reset: got %s, want %s", got, headHash)
	}

	if _, err := os.Stat(filepath.Join(lease2.WorkingTree(), barPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected bar.yaml to be gone after reset, got err=%v", err)
	}

	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("Return lease2: %v", err)
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

// BenchmarkLease measures the cost of a Lease/Return cycle against a real
// remote repository. It uses golang/go at master as a representative large
// repository (~15k files, ~400MB). Set GITHUB_TOKEN to run.
//
// Example:
//
//	GITHUB_TOKEN=$(gh auth token) \
//	  go test -bench BenchmarkLease -benchtime 10x -timeout 30m
func BenchmarkLease(b *testing.B) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		b.Skip("GITHUB_TOKEN not set")
	}

	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(token), "bench", nil)
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	res := &githubreconciler.Resource{
		Owner: "golang",
		Repo:  "go",
		Ref:   "master",
		Path:  "README.md",
		Type:  githubreconciler.ResourceTypePath,
	}

	// Warm up: pay the initial clone cost outside the timer.
	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		b.Fatalf("warmup Lease: %v", err)
	}
	if err := lease.Return(ctx); err != nil {
		b.Fatalf("warmup Return: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		lease, err := mgr.Lease(ctx, res)
		if err != nil {
			b.Fatalf("Lease: %v", err)
		}
		if err := lease.Return(ctx); err != nil {
			b.Fatalf("Return: %v", err)
		}
	}
}

type staticTokenSource string

func (s staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: string(s)}, nil
}
