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

// forEachBackend runs the test body against both clone/fetch transports, so
// the git CLI backend cannot silently drift from the go-git behavior it
// mirrors.
func forEachBackend(t *testing.T, fn func(t *testing.T, opts ...Option)) {
	t.Run("gogit", func(t *testing.T) { fn(t) })
	t.Run("gitcli", func(t *testing.T) { fn(t, WithGitCLI()) })
}

func TestLeaseLifecycle(t *testing.T) { forEachBackend(t, testLeaseLifecycle) }

func testLeaseLifecycle(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
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

	// A path whose intermediate directory does not exist must also be
	// treated as a non-existent path, not surfaced as an error.
	missingDir := *res
	missingDir.Path = "nonexistent/missing.yaml"
	leaseMissingDir, err := mgr.Lease(ctx, &missingDir)
	if err != nil {
		t.Fatalf("Lease missing directory path: %v", err)
	}
	if leaseMissingDir.PathExists() {
		t.Fatalf("expected missing directory path to report false")
	}
	if err := leaseMissingDir.Return(ctx); err != nil {
		t.Fatalf("returning missing directory lease: %v", err)
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

func TestMakeAndPushChanges(t *testing.T) { forEachBackend(t, testMakeAndPushChanges) }

func testMakeAndPushChanges(t *testing.T, opts ...Option) {
	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
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

	// Dirty the worktree before calling MakeAndPushChanges, simulating
	// external commands that modify files in-place.
	fooPath := filepath.Join(lease.WorkingTree(), "packages", "foo.yaml")
	if err := os.WriteFile(fooPath, []byte("name: foo-updated"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	branchName := "clonemanager/test-branch"
	barPath := filepath.ToSlash(filepath.Join("packages", "bar.yaml"))

	if err := lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *git.Worktree) (string, error) {
		// The updateFn context must carry the worktree for code that cannot
		// receive it as a parameter (e.g. result validators).
		if ctxWT, ok := WorktreeFromContext(ctx); !ok || ctxWT != wt {
			return "", fmt.Errorf("WorktreeFromContext inside updateFn: got = (%p, %v), want = (%p, true)", ctxWT, ok, wt)
		}

		// Verify the worktree has our changes when the updateFn is called.
		status, err := wt.Status()
		if err != nil {
			return "", fmt.Errorf("Status: %w", err)
		}
		if status.IsClean() {
			return "", fmt.Errorf("expected clean worktree inside updateFn, got: %v", status)
		}

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

	branchRef, err := originRepo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}

	// Under the stage-once contract, commitChanges runs `git add -A`, so the
	// pushed commit captures both bar.yaml (written and staged inside updateFn)
	// and foo.yaml (dirtied before the turn and never explicitly staged). Assert
	// the tree holds both, locking the new contract end-to-end.
	pushedCommit, err := originRepo.CommitObject(branchRef.Hash())
	if err != nil {
		t.Fatalf("load pushed commit: %v", err)
	}
	wantFiles := map[string]string{
		filepath.ToSlash(filepath.Join("packages", "foo.yaml")): "name: foo-updated",
		barPath: "name: bar",
	}
	for path, want := range wantFiles {
		f, err := pushedCommit.File(path)
		if err != nil {
			t.Fatalf("pushed commit missing %q: %v", path, err)
		}
		got, err := f.Contents()
		if err != nil {
			t.Fatalf("read %q from pushed commit: %v", path, err)
		}
		if got != want {
			t.Errorf("%q in pushed commit: got %q, want %q", path, got, want)
		}
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

func TestMakeAndPushChanges_NothingToCommit(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesNothingToCommit)
}

func testMakeAndPushChangesNothingToCommit(t *testing.T, opts ...Option) {
	ctx := t.Context()

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
	t.Cleanup(func() {
		// t.Context() is canceled before cleanups run; Return's reset must
		// survive that (context.WithoutCancel) instead of discarding the
		// clone, so this doubles as the canceled-context regression test.
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	err = lease.MakeAndPushChanges(ctx, "clonemanager/no-op-branch", func(_ context.Context, _ *git.Worktree) (string, error) {
		// updateFn returns successfully but makes no changes to the worktree
		return "this commit message should never be used", nil
	})
	if !errors.Is(err, ErrNothingToCommit) {
		t.Errorf("MakeAndPushChanges with no changes: got = %v, wanted = ErrNothingToCommit", err)
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
func TestFIFOPoolBehavior(t *testing.T) { forEachBackend(t, testFIFOPoolBehavior) }

func testFIFOPoolBehavior(t *testing.T, opts ...Option) {
	ctx := context.Background()

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

// TestLeaseRefDeepHistory drives LeaseRef with WithCommitDepth over a
// file:// remote — the URL form under which git honors --depth, unlike the
// plain-path remotes used elsewhere in this suite — so both backends
// exercise real shallow clones and deepening fetches, the production shape.
func TestLeaseRefDeepHistory(t *testing.T) { forEachBackend(t, testLeaseRefDeepHistory) }

func testLeaseRefDeepHistory(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, firstHash := initTestRepo(t)
	commitToTestRepo(t, repoDir, "second.yaml")
	commitToTestRepo(t, repoDir, "third.yaml")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Ref:   "master",
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return "file://" + repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	// Depth 3 covers all three commits; the merge-base walk (depth-1 = 2
	// first-parent hops from HEAD) must land on the initial commit's parent
	// slot, i.e. BaseCommit is the initial commit.
	lease, err := mgr.LeaseRef(ctx, res, "master", WithCommitDepth(3))
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	if got := lease.BaseCommit().String(); got != firstHash {
		t.Errorf("BaseCommit: got %s, want %s", got, firstHash)
	}
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return: %v", err)
	}

	// Re-lease at depth 1 with an unmoved tip: the pooled clone is reused
	// and the shallow boundary handling must not break the lease.
	lease2, err := mgr.LeaseRef(ctx, res, "master")
	if err != nil {
		t.Fatalf("LeaseRef reuse: %v", err)
	}
	if lease2.SHA() == "" || !lease2.PathExists() {
		t.Errorf("reused lease: SHA=%q PathExists=%v, want non-empty SHA and true", lease2.SHA(), lease2.PathExists())
	}
	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("Return reuse: %v", err)
	}
}

// TestLeaseRefNonBranchRef leases a refs/... ref that is not advertised at
// clone time, exercising the clone-default-branch-then-fetch path.
func TestLeaseRefNonBranchRef(t *testing.T) { forEachBackend(t, testLeaseRefNonBranchRef) }

func testLeaseRefNonBranchRef(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)

	// Point a PR-style ref at HEAD, as GitHub does for refs/pull/N/head.
	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	prRef := plumbing.ReferenceName("refs/pull/1/head")
	if err := origin.Storer.SetReference(plumbing.NewHashReference(prRef, plumbing.NewHash(headHash))); err != nil {
		t.Fatalf("SetReference: %v", err)
	}

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Type:  githubreconciler.ResourceTypePullRequest,
	}

	repoURL = func(*githubreconciler.Resource) string { return "file://" + repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, prRef.String())
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	if got := lease.SHA(); got != headHash {
		t.Errorf("SHA: got %s, want %s", got, headHash)
	}
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return: %v", err)
	}
}

// TestLeaseAfterForcePush moves the remote branch backward (history rewrite)
// and verifies a pooled clone follows it to the new tip.
func TestLeaseAfterForcePush(t *testing.T) { forEachBackend(t, testLeaseAfterForcePush) }

func testLeaseAfterForcePush(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, firstHash := initTestRepo(t)
	commitToTestRepo(t, repoDir, "second.yaml")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Ref:   "master",
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return "file://" + repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	tip := lease.SHA()
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return: %v", err)
	}

	// Rewind master to the initial commit, simulating a force push.
	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	if err := origin.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), plumbing.NewHash(firstHash))); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	wt, err := origin.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
		t.Fatalf("Reset origin: %v", err)
	}

	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after force push: %v", err)
	}
	if got := lease2.SHA(); got != firstHash {
		t.Errorf("SHA after force push: got %s, want %s (was %s)", got, firstHash, tip)
	}
	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("Return after force push: %v", err)
	}
}

// TestReturnClearsInfoExclude verifies Return strips a .git/info/exclude a
// prior lease left in the pooled clone, so its ignore rules cannot leak into a
// later lease. It also locks the commit behavior: a file matching the (now
// cleared) exclude is committed, not silently dropped — which would also catch
// a future go-git that starts honoring .git/info/exclude in Status/Add.
func TestReturnClearsInfoExclude(t *testing.T) { forEachBackend(t, testReturnClearsInfoExclude) }

func testReturnClearsInfoExclude(t *testing.T, opts ...Option) {
	ctx := t.Context()

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

	// Lease 1 poisons .git/info/exclude, as untrusted code with worktree write
	// access could, then returns the clone to the pool.
	lease1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	dir := lease1.WorkingTree()
	excl := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(excl, []byte("*\n"), 0o644); err != nil {
		t.Fatalf("poison exclude: %v", err)
	}
	if err := lease1.Return(ctx); err != nil {
		t.Fatalf("Return 1: %v", err)
	}

	// Lease 2 reuses the same pooled clone; the poisoned exclude must be gone.
	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 2: %v", err)
	}
	if lease2.WorkingTree() != dir {
		t.Fatalf("expected pooled clone reuse: got %s, want %s", lease2.WorkingTree(), dir)
	}
	if _, err := os.Stat(excl); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".git/info/exclude persisted across Return: err=%v, want removed", err)
	}

	// A file matching the cleared exclude must still land in the commit.
	newFile := filepath.ToSlash(filepath.Join("packages", "bar.yaml"))
	branch := "clonemanager/exclude-test"
	if err := lease2.MakeAndPushChanges(ctx, branch, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), newFile), []byte("name: bar"), 0o644); err != nil {
			return "", err
		}
		return "add bar", nil
	}); err != nil {
		t.Fatalf("MakeAndPushChanges: %v", err)
	}
	if err := lease2.Return(ctx); err != nil {
		t.Fatalf("Return 2: %v", err)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	ref, err := origin.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	commit, err := origin.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	if _, err := commit.File(newFile); err != nil {
		t.Errorf("%q missing from pushed commit (silently dropped by a poisoned exclude?): %v", newFile, err)
	}
}

// TestReturnInfoExcludeRemovalStaysInClone verifies the .git/info/exclude
// cleanup cannot be redirected outside the clone: a lease that replaces
// .git/info with a symlink to an external directory must not cause Return to
// delete <external>/exclude. os.Root confines the removal, so the escaping
// symlink is refused (Return errors and the clone is discarded) and the
// external file survives.
func TestReturnInfoExcludeRemovalStaysInClone(t *testing.T) {
	forEachBackend(t, testReturnInfoExcludeRemovalStaysInClone)
}

func testReturnInfoExcludeRemovalStaysInClone(t *testing.T, opts ...Option) {
	ctx := t.Context()

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

	// An external directory with a file the escaping symlink would target.
	external := t.TempDir()
	victim := filepath.Join(external, "exclude")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	// Replace .git/info with a symlink to the external directory.
	info := filepath.Join(lease.WorkingTree(), ".git", "info")
	if err := os.RemoveAll(info); err != nil {
		t.Fatalf("RemoveAll .git/info: %v", err)
	}
	if err := os.Symlink(external, info); err != nil {
		t.Fatalf("Symlink .git/info: %v", err)
	}

	// Return's cleanup must refuse to traverse the escaping symlink.
	_ = lease.Return(ctx)

	if _, err := os.Stat(victim); err != nil {
		t.Errorf("external exclude was removed through a symlinked .git/info: %v; want it untouched", err)
	}
}

// commitToTestRepo advances the test repo's master branch with a new commit.
func commitToTestRepo(t *testing.T, repoDir, name string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	rel := filepath.ToSlash(filepath.Join("packages", name))
	if err := os.WriteFile(filepath.Join(repoDir, rel), []byte("name: "+name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wt.Add(rel); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := wt.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestMaxFetchesDiscardsClone(t *testing.T) { forEachBackend(t, testMaxFetchesDiscardsClone) }

func testMaxFetchesDiscardsClone(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, append(opts, WithMaxFetches(2))...)
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

	// Leases whose fetch finds the ref already up to date don't consume the
	// bound: the clone survives arbitrarily many of them.
	dir := ""
	for i := range 3 {
		lease, err := mgr.Lease(ctx, res)
		if err != nil {
			t.Fatalf("Lease %d: %v", i, err)
		}
		if dir == "" {
			dir = lease.WorkingTree()
		} else if got := lease.WorkingTree(); got != dir {
			t.Fatalf("Lease %d working tree: got %s, want %s (reused clone)", i, got, dir)
		}
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return %d: %v", i, err)
		}
	}

	// Advancing the remote makes the next lease's fetch transfer a pack,
	// consuming one of the two budgeted fetches. The clone survives: the
	// counter accumulates across leases rather than tracking only the most
	// recent one.
	commitToTestRepo(t, repoDir, "bar.yaml")
	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after first update: %v", err)
	}
	if got := lease.WorkingTree(); got != dir {
		t.Fatalf("Lease after first update working tree: got %s, want %s (reused clone)", got, dir)
	}
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return after first update: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected clone to survive first transferring fetch, got err=%v", err)
	}

	// A second remote advance exhausts the bound and discards the clone on
	// return.
	commitToTestRepo(t, repoDir, "baz.yaml")
	lease, err = mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after second update: %v", err)
	}
	if got := lease.WorkingTree(); got != dir {
		t.Fatalf("Lease after second update working tree: got %s, want %s (reused clone)", got, dir)
	}
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return after second update: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected discarded clone dir to be removed, got err=%v", err)
	}

	// The next lease gets a fresh clone.
	lease, err = mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease after discard: %v", err)
	}
	if got := lease.WorkingTree(); got == dir {
		t.Fatalf("Lease after discard working tree: got reused %s, want a fresh clone", got)
	}
	_ = lease.Return(ctx)
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

	b.Run("gogit", func(b *testing.B) { benchmarkLease(b, token) })
	b.Run("gitcli", func(b *testing.B) { benchmarkLease(b, token, WithGitCLI()) })
}

func benchmarkLease(b *testing.B, token string, opts ...Option) {
	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(token), "bench", nil, opts...)
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
