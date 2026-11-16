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
	"os/exec"
	"path/filepath"
	"strings"
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
	t.Run("gogit", func(t *testing.T) { fn(t, WithGoGit()) })
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
	if got := lease.Root().Name(); got != workingDir {
		t.Fatalf("Root().Name(): got = %q, want = %q", got, workingDir)
	}
	if _, err := lease.Root().Stat(res.Path); err != nil {
		t.Fatalf("Root().Stat(%q): got = %v, want = nil", res.Path, err)
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

// TestMakeAndPushChanges_RefusesOverwritingHumanCommit verifies a human's
// commit to the PR branch, added while updateFn is still running, survives
// the eventual force push.
func TestMakeAndPushChanges_RefusesOverwritingHumanCommit(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRefusesOverwritingHumanCommit)
}

func testMakeAndPushChangesRefusesOverwritingHumanCommit(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	// Lease the PR branch itself (not the default branch), matching how
	// metapathreconciler leases an existing PR's head branch for iteration.
	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	// A human pushes to the branch while the agent's updateFn below is
	// (conceptually) still running.
	humanCommit := commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	// A non-bare origin refuses a push to whichever branch its own worktree
	// has checked out; switch off it, as a real bare GitHub remote would
	// never have this restriction in the first place.
	checkoutBranch(t, repoDir, "master")

	err = lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	})
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("MakeAndPushChanges over a human commit: got = %v, wanted = ErrBranchMoved", err)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	branchRef, err := origin.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	if branchRef.Hash() != humanCommit {
		t.Errorf("branch tip after refused push: got %s, want the preserved human commit %s", branchRef.Hash(), humanCommit)
	}
}

// TestMakeAndPushChanges_RefusesOverwritingHumanCommit_FullyQualifiedLease is
// TestMakeAndPushChanges_RefusesOverwritingHumanCommit with the lease taken
// against the fully qualified ref ("refs/heads/...") while MakeAndPushChanges
// is called with the bare branch name: the guard must compare canonical ref
// names, since comparing raw strings would never match these two forms,
// silently disabling it.
func TestMakeAndPushChanges_RefusesOverwritingHumanCommit_FullyQualifiedLease(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRefusesOverwritingHumanCommitFullyQualifiedLease)
}

func testMakeAndPushChangesRefusesOverwritingHumanCommitFullyQualifiedLease(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	// Lease the fully qualified ref; MakeAndPushChanges below is called with
	// the bare branch name for the same branch.
	lease, err := mgr.LeaseRef(ctx, res, "refs/heads/"+branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	humanCommit := commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	checkoutBranch(t, repoDir, "master")

	err = lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	})
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("MakeAndPushChanges over a human commit (fully qualified lease ref): got = %v, wanted = ErrBranchMoved", err)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	branchRef, err := origin.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	if branchRef.Hash() != humanCommit {
		t.Errorf("branch tip after refused push: got %s, want the preserved human commit %s", branchRef.Hash(), humanCommit)
	}
}

// TestMakeAndPushChanges_GuardMatchesActualPushTarget covers the mix the
// fully-qualified-lease test above doesn't: lease and branchName both already
// fully qualified ("refs/heads/..."). refuseIfOverwritesOthers must decide
// using the same ref createFreshBranch is about to push to, not a value it
// resolves from branchName independently: createFreshBranch always treats
// branchName as a bare name (plumbing.NewBranchReferenceName prepends
// "refs/heads/" unconditionally, pre-existing and out of scope here), so for
// this input the two disagree. Deciding from branchName would match the
// lease, require a ref that can never exist, and permanently misreport
// ErrBranchMoved; deciding from the actual push target correctly treats it
// as unprotected instead, same as leasing some other ref entirely.
func TestMakeAndPushChanges_GuardMatchesActualPushTarget(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesGuardMatchesActualPushTarget)
}

func testMakeAndPushChangesGuardMatchesActualPushTarget(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	qualified := "refs/heads/" + branchName
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	// Lease and push both use the fully qualified form.
	lease, err := mgr.LeaseRef(ctx, res, qualified)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	if err := lease.MakeAndPushChanges(ctx, qualified, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	}); err != nil {
		t.Fatalf("MakeAndPushChanges with a fully qualified branchName matching the lease: got %v, want nil (no drift occurred)", err)
	}
}

// TestMakeAndPushChanges_RefusesOverwritingOwnPriorCommit verifies the guard
// refuses even when the branch moved under the manager's own identity (e.g.
// an overlapping run of the same bot), not just a human's. Under this
// package's exclusive-lease model that should not happen legitimately, so
// there is no special case for it: any drift at all is refused, which is
// both simpler and safer than trying to distinguish whose commit it is.
func TestMakeAndPushChanges_RefusesOverwritingOwnPriorCommit(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRefusesOverwritingOwnPriorCommit)
}

func testMakeAndPushChangesRefusesOverwritingOwnPriorCommit(t *testing.T, opts ...Option) {
	ctx := t.Context()

	identity := "clonemanager-test"
	mgr, err := New(ctx, staticTokenSource(""), identity, nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	priorCommit := commitToTestRepoAs(t, repoDir, "other-run.yaml", identity, identity+"@chainguard.dev")
	checkoutBranch(t, repoDir, "master")

	err = lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	})
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("MakeAndPushChanges over the bot's own prior commit: got = %v, wanted = ErrBranchMoved", err)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	branchRef, err := origin.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	if branchRef.Hash() != priorCommit {
		t.Errorf("branch tip after refused push: got %s, want the preserved prior commit %s", branchRef.Hash(), priorCommit)
	}
}

// TestMakeAndPushChanges_PRIterationNoDrift covers the case
// TestMakeAndPushChanges never did: leasing the exact branch being pushed
// back to (branchName == leasedRef), the same shape metapathreconciler uses
// to iterate on an existing PR. With no drift, the RequireRemoteRefs
// constraint refuseIfOverwritesOthers returns must match the actual remote
// state and let the push through, and the recheck fetch must not count
// toward the clone's fetch bound: its destination ref is always deleted and
// recreated, so backend.fetch's grew signal is always true there regardless
// of whether a pack actually transferred.
func TestMakeAndPushChanges_PRIterationNoDrift(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesPRIterationNoDrift)
}

func testMakeAndPushChangesPRIterationNoDrift(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	fetchesBeforePush := lease.clone.fetches

	if err := lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	}); err != nil {
		t.Fatalf("MakeAndPushChanges with no drift: %v", err)
	}

	if lease.clone.fetches != fetchesBeforePush {
		t.Errorf("clone.fetches after recheck fetch: got %d, want %d (recheck fetch must not count toward the fetch bound)", lease.clone.fetches, fetchesBeforePush)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	branchRef, err := origin.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	if branchRef.Hash().String() == headHash {
		t.Errorf("branch tip after push: still at %s, want it to have advanced", headHash)
	}
	pushedCommit, err := origin.CommitObject(branchRef.Hash())
	if err != nil {
		t.Fatalf("load pushed commit: %v", err)
	}
	if _, err := pushedCommit.File(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Errorf("pushed commit missing bot.yaml: %v", err)
	}
}

// TestMakeAndPushChanges_RefusesPushTimeLeaseRejection exercises the half of
// the TOCTOU guard the other refuse tests above cannot reach: they land the
// drift before MakeAndPushChanges starts, so refuseIfOverwritesOthers' own
// recheck fetch catches it locally. This test lands the drift after that
// recheck but before forcePushBranch runs.
//
// The resulting rejection is go-git's own client-side RequireRemoteRefs check
// against a fresh advertisement, not a round trip to a real git-receive-pack
// -- go-git offers no way to narrow the window further. It still proves a
// real TOCTOU gap end to end, and is the only test here that drives
// isRemoteRefLeaseRejected's "required to be" marker against a real error;
// the other two, server-only markers are covered synthetically by
// TestIsRemoteRefLeaseRejected.
func TestMakeAndPushChanges_RefusesPushTimeLeaseRejection(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRefusesPushTimeLeaseRejection)
}

func testMakeAndPushChangesRefusesPushTimeLeaseRejection(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	// The recheck fetch runs here, against the remote as it stands right now
	// -- no drift yet, so this succeeds and returns the SHA the push below
	// must require the remote to still be at.
	requireRemote, err := lease.refuseIfOverwritesOthers(ctx, ref)
	if err != nil {
		t.Fatalf("refuseIfOverwritesOthers before drift: %v", err)
	}

	// Land the drift in the window the local recheck already passed: a human
	// pushes to the branch after refuseIfOverwritesOthers observed it, but
	// before forcePushBranch's push reaches the remote.
	checkoutBranch(t, repoDir, branchName)
	humanCommit := commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	checkoutBranch(t, repoDir, "master")

	err = lease.manager.forcePushBranch(ctx, lease.clone.repo, ref, lease.remoteURL, requireRemote)
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("forcePushBranch over a lease rejected at push time: got = %v, wanted = ErrBranchMoved", err)
	}

	origin, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen origin: %v", err)
	}
	branchRef, err := origin.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		t.Fatalf("Reference lookup: %v", err)
	}
	if branchRef.Hash() != humanCommit {
		t.Errorf("branch tip after rejected push: got %s, want the preserved human commit %s", branchRef.Hash(), humanCommit)
	}
}

// TestIsRemoteRefLeaseRejected covers the two markers
// TestMakeAndPushChanges_RefusesPushTimeLeaseRejection cannot reach end to
// end: "incorrect old value provided" and "cannot lock ref" come from a real
// git-receive-pack's ref-transaction hook, not from go-git's own client-side
// RequireRemoteRefs check, and this test suite has no way to drive an actual
// server into producing them. These are synthetic error strings instead.
func TestIsRemoteRefLeaseRejected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("connection reset by peer"), false},
		{"required to be (go-git client-side check)", errors.New(`remote ref refs/heads/foo required to be abc123 but is def456`), true},
		{"incorrect old value provided (git-receive-pack)", errors.New(`! [remote rejected] foo -> foo (failed to update ref: incorrect old value provided)`), true},
		{"cannot lock ref (git-receive-pack)", errors.New(`! [remote rejected] foo -> foo (cannot lock ref 'refs/heads/foo': is at abc123 but expected def456)`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRemoteRefLeaseRejected(tt.err); got != tt.want {
				t.Errorf("isRemoteRefLeaseRejected(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestClassifyPushError covers the requireRemote gate directly, for the same
// reason TestIsRemoteRefLeaseRejected covers its markers synthetically: a
// fresh branch's push (requireRemote zero) never carries a RequireRemoteRefs
// constraint, so this suite has no way to make a real push produce a
// lease-marker error under that condition to exercise end to end.
func TestClassifyPushError(t *testing.T) {
	leaseErr := errors.New(`! [remote rejected] foo -> foo (cannot lock ref 'refs/heads/foo': is at abc123 but expected def456)`)

	t.Run("lease conflict", func(t *testing.T) {
		got := classifyPushError(plumbing.NewHash("abc123"), leaseErr)
		if !errors.Is(got, ErrBranchMoved) {
			t.Errorf("classifyPushError with requireRemote set = %v, want ErrBranchMoved", got)
		}
	})

	t.Run("fresh branch, same marker text", func(t *testing.T) {
		got := classifyPushError(plumbing.ZeroHash, leaseErr)
		if errors.Is(got, ErrBranchMoved) {
			t.Errorf("classifyPushError with requireRemote zero = %v, want anything but ErrBranchMoved: a fresh branch has no lease constraint to violate", got)
		}
	})
}

// TestMakeAndPushChanges_RecheckRefEscapeIsRefused verifies
// refuseIfOverwritesOthers' recheck fetch cannot be redirected outside the
// clone: updateFn runs on untrusted worktree content before this fetch, and
// could replace .git/refs/clonemanager-recheck with a symlink to write the
// fetched ref somewhere else on the host. os.Root confines the symlink
// removal that precedes the fetch, so the escaping symlink is cleared rather
// than followed, and the external target is left untouched.
func TestMakeAndPushChanges_RecheckRefEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckRefEscapeIsRefused)
}

func testMakeAndPushChangesRecheckRefEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Return(ctx); err != nil {
			t.Fatalf("Return: %v", err)
		}
	})

	// An external directory the escaping symlink would target, standing in
	// for an arbitrary host path outside the clone.
	external := t.TempDir()

	err = lease.MakeAndPushChanges(ctx, branchName, func(_ context.Context, wt *git.Worktree) (string, error) {
		// Simulate updateFn's untrusted content replacing the recheck ref
		// path with a symlink before refuseIfOverwritesOthers' fetch runs.
		recheck := filepath.Join(lease.WorkingTree(), ".git", "refs", "clonemanager-recheck")
		if err := os.MkdirAll(filepath.Dir(recheck), 0o755); err != nil {
			return "", fmt.Errorf("MkdirAll: %w", err)
		}
		if err := os.Symlink(external, recheck); err != nil {
			return "", fmt.Errorf("Symlink: %w", err)
		}

		if err := os.WriteFile(filepath.Join(wt.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
			return "", fmt.Errorf("WriteFile: %w", err)
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
			return "", fmt.Errorf("Add: %w", err)
		}
		return "agent change", nil
	})
	if err != nil {
		t.Fatalf("MakeAndPushChanges with a symlinked recheck ref path: %v", err)
	}

	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir external: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("external directory after push: got %d entr(y/ies), want empty; the recheck fetch wrote through the symlink instead of it being cleared", len(entries))
	}
}

// TestMakeAndPushChanges_RecheckRefParentEscapeIsRefused covers the case
// TestMakeAndPushChanges_RecheckRefEscapeIsRefused doesn't: .git/refs itself
// (a parent of the recheck ref, not the leaf) replaced with a symlink. The
// guard must clear the whole path with Remove, not just check the leaf for a
// symlink: an escaping parent surfaces as a non-ErrNotExist error from
// Remove, which only a path-wide clear can catch.
func TestMakeAndPushChanges_RecheckRefParentEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckRefParentEscapeIsRefused)
}

func testMakeAndPushChangesRecheckRefParentEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		// The test corrupts .git/refs below; Return's own reset over that
		// state can legitimately fail, but Return already discards the
		// clone whenever reset fails, so there's nothing left to leak.
		_ = lease.Return(ctx)
	})

	// Drive commitChanges directly rather than through the full
	// MakeAndPushChanges/updateFn path: go-git's own worktree confinement
	// already refuses a commit that has to update a branch ref through a
	// symlinked .git/refs, so updateFn planting that symlink itself would
	// never reach refuseIfOverwritesOthers at all. Committing first and
	// corrupting refs after isolates the guard this test targets.
	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	// An external directory standing in for an arbitrary host path outside
	// the clone.
	external := t.TempDir()
	refsDir := filepath.Join(lease.WorkingTree(), ".git", "refs")
	if err := os.RemoveAll(refsDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.Symlink(external, refsDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, err := lease.refuseIfOverwritesOthers(ctx, ref); err == nil || !strings.Contains(err.Error(), "clearing recheck ref path") {
		t.Fatalf("refuseIfOverwritesOthers with a symlinked .git/refs: got %v, want an error clearing the recheck ref path", err)
	}

	if _, err := os.Lstat(filepath.Join(external, "clonemanager-recheck")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("external directory after push: clonemanager-recheck exists (err=%v), want absent; the recheck fetch wrote through the symlinked parent instead of the escape being refused", err)
	}
}

// TestMakeAndPushChanges_RecheckFetchObjectEscapeIsRefused covers a
// different write than the recheck ref: the fetch's received objects.
// .git/objects is a real directory the clone depends on (unlike the
// throwaway recheck ref), so the guard must clear only an actual symlink
// there and replace it with a real, empty directory rather than leaving it
// absent: a missing .git/objects makes git itself refuse the clone as "not a
// git repository". Drift is real here (not just asserted by the test) so
// the fetch has a new commit's objects to write, exercising the path a
// no-drift fetch would skip.
func TestMakeAndPushChanges_RecheckFetchObjectEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckFetchObjectEscapeIsRefused)
}

func testMakeAndPushChangesRecheckFetchObjectEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		// The test corrupts .git/objects below; Return's own reset over that
		// state can legitimately fail, but Return already discards the
		// clone whenever reset fails, so there's nothing left to leak.
		_ = lease.Return(ctx)
	})

	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	// Land real drift: a human commit the recheck fetch must actually
	// download, not just a hash the test asserts differs.
	checkoutBranch(t, repoDir, branchName)
	commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	checkoutBranch(t, repoDir, "master")

	// An external directory standing in for an arbitrary host path outside
	// the clone.
	external := t.TempDir()
	objectsDir := filepath.Join(lease.WorkingTree(), ".git", "objects")
	if err := os.RemoveAll(objectsDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.Symlink(external, objectsDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err = lease.refuseIfOverwritesOthers(ctx, ref)
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("refuseIfOverwritesOthers with symlinked .git/objects: got %v, want ErrBranchMoved (the human commit is real drift)", err)
	}

	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir external: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("external directory after push: got %d entr(y/ies), want empty; the recheck fetch wrote objects through the symlink instead of it being cleared", len(entries))
	}
}

// TestMakeAndPushChanges_RecheckFetchPackEscapeIsRefused covers the case
// TestMakeAndPushChanges_RecheckFetchObjectEscapeIsRefused doesn't:
// .git/objects/pack alone replaced with a symlink while .git/objects itself
// stays a real, legitimate directory. The guard must check this path
// directly, not rely on .git/objects alone being real: the two are checked
// independently, and a parent being fine says nothing about a child.
func TestMakeAndPushChanges_RecheckFetchPackEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckFetchPackEscapeIsRefused)
}

func testMakeAndPushChangesRecheckFetchPackEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		// The test corrupts .git/objects/pack below; Return's own reset over
		// that state can legitimately fail, but Return already discards the
		// clone whenever reset fails, so there's nothing left to leak.
		_ = lease.Return(ctx)
	})

	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	// Land drift large enough that git's own default (transfer.unpackLimit
	// 100) keeps it as a pack instead of unpacking into loose objects under
	// .git/objects/<xx>/: a single small commit would never touch
	// .git/objects/pack/ at all, so this is what makes the test actually
	// exercise the path it targets for the CLI backend, rather than passing
	// by the unrelated accident of a small drift. The clone's own .git/config
	// can't be used to force this instead (every git invocation here resets
	// it to a fixed minimal config first; see resetGitConfig).
	checkoutBranch(t, repoDir, branchName)
	driftRepo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	driftWT, err := driftRepo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for i := range 150 {
		name := filepath.ToSlash(filepath.Join("packages", fmt.Sprintf("drift-%d.yaml", i)))
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(fmt.Sprintf("name: drift-%d", i)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := driftWT.Add(name); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if _, err := driftWT.Commit("land drift", &git.CommitOptions{
		Author: &object.Signature{Name: "A Human", Email: "human@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	checkoutBranch(t, repoDir, "master")

	// An external directory standing in for an arbitrary host path outside
	// the clone. .git/objects itself is left alone, real and already
	// populated by the initial clone; only the pack subdirectory is replaced.
	external := t.TempDir()
	packDir := filepath.Join(lease.WorkingTree(), ".git", "objects", "pack")
	if err := os.RemoveAll(packDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.Symlink(external, packDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err = lease.refuseIfOverwritesOthers(ctx, ref)
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("refuseIfOverwritesOthers with symlinked .git/objects/pack: got %v, want ErrBranchMoved (the human commit is real drift)", err)
	}

	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir external: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("external directory after push: got %d entr(y/ies), want empty; the recheck fetch wrote a pack through the symlink instead of it being cleared", len(entries))
	}
}

// TestMakeAndPushChanges_RecheckFetchFanoutEscapeIsRefused covers a loose
// object landing through a fan-out bucket (.git/objects/<xx>) rather than
// .git/objects or .git/objects/pack themselves: a bucket only exists once
// some object has hashed into it, so updateFn can symlink one, or all 256,
// to a host path before any real object lands there -- a path the earlier
// two tests, which only check .git/objects and .git/objects/pack by name,
// don't exercise.
func TestMakeAndPushChanges_RecheckFetchFanoutEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckFetchFanoutEscapeIsRefused)
}

func testMakeAndPushChangesRecheckFetchFanoutEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		// The test corrupts .git/objects/<xx> below; Return's own reset over
		// that state can legitimately fail, but Return already discards the
		// clone whenever reset fails, so there's nothing left to leak.
		_ = lease.Return(ctx)
	})

	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	// A single small commit, unlike the pack test's 150 files: it stays
	// under git's transfer.unpackLimit default (100) and is unpacked into a
	// loose object under some .git/objects/<xx>/, which is what this test
	// needs to land through a fan-out bucket rather than a pack.
	checkoutBranch(t, repoDir, branchName)
	commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	checkoutBranch(t, repoDir, "master")

	// An external directory standing in for an arbitrary host path outside
	// the clone. Every possible fan-out bucket is symlinked to it, since a
	// bucket only exists once an object hashes into it and the drift's
	// object hash isn't known ahead of time; .git/objects itself is left
	// alone, real and already populated by the initial clone.
	external := t.TempDir()
	objectsDir := filepath.Join(lease.WorkingTree(), ".git", "objects")
	for i := range 256 {
		bucket := fmt.Sprintf("%02x", i)
		p := filepath.Join(objectsDir, bucket)
		if err := os.RemoveAll(p); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		if err := os.Symlink(external, p); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}

	_, err = lease.refuseIfOverwritesOthers(ctx, ref)
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("refuseIfOverwritesOthers with symlinked .git/objects/<xx>: got %v, want ErrBranchMoved (the human commit is real drift)", err)
	}

	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("ReadDir external: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("external directory after push: got %d entr(y/ies), want empty; the recheck fetch wrote a loose object through a fan-out bucket symlink instead of it being cleared", len(entries))
	}
}

// TestMakeAndPushChanges_RecheckFetchHeadEscapeIsRefused covers a write the
// object-store hardening above doesn't reach: git fetch's own write to
// .git/FETCH_HEAD. Unlike .git/objects, nothing in this package reads
// FETCH_HEAD and the clone doesn't depend on it existing, so the fix is
// suppressing the write outright (--no-write-fetch-head) rather than
// detecting and clearing a symlink beforehand.
func TestMakeAndPushChanges_RecheckFetchHeadEscapeIsRefused(t *testing.T) {
	forEachBackend(t, testMakeAndPushChangesRecheckFetchHeadEscapeIsRefused)
}

func testMakeAndPushChangesRecheckFetchHeadEscapeIsRefused(t *testing.T, opts ...Option) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	repoDir, headHash := initTestRepo(t)
	branchName := "clonemanager/pr-branch"
	checkoutTestBranch(t, repoDir, headHash)
	checkoutBranch(t, repoDir, "master")

	res := &githubreconciler.Resource{
		Owner: "tests",
		Repo:  repoDir,
		Path:  filepath.ToSlash(filepath.Join("packages", "foo.yaml")),
		Type:  githubreconciler.ResourceTypePath,
	}

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.LeaseRef(ctx, res, branchName)
	if err != nil {
		t.Fatalf("LeaseRef: %v", err)
	}
	t.Cleanup(func() {
		// The test corrupts .git/FETCH_HEAD below; Return's own reset over
		// that state can legitimately fail, but Return already discards the
		// clone whenever reset fails, so there's nothing left to leak.
		_ = lease.Return(ctx)
	})

	ref, err := lease.createFreshBranch(branchName)
	if err != nil {
		t.Fatalf("createFreshBranch: %v", err)
	}
	worktree, err := lease.clone.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree.Filesystem.Root(), "packages", "bot.yaml"), []byte("name: bot"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("packages", "bot.yaml"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := lease.manager.commitChanges(ctx, lease.clone.repo, nil, nil, "agent change"); err != nil {
		t.Fatalf("commitChanges: %v", err)
	}

	checkoutBranch(t, repoDir, branchName)
	commitToTestRepoAs(t, repoDir, "human.yaml", "A Human", "human@example.com")
	checkoutBranch(t, repoDir, "master")

	// An external file standing in for an arbitrary host path outside the
	// clone, pre-populated so a silent overwrite is detectable: FETCH_HEAD is
	// a single file, not a directory the clone must find real afterward, so
	// unlike the object-store tests above there's no empty-replacement to
	// check for, only whether the write happened at all.
	external := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(external, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("WriteFile external: %v", err)
	}
	fetchHead := filepath.Join(lease.WorkingTree(), ".git", "FETCH_HEAD")
	if err := os.RemoveAll(fetchHead); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.Symlink(external, fetchHead); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err = lease.refuseIfOverwritesOthers(ctx, ref)
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("refuseIfOverwritesOthers with symlinked .git/FETCH_HEAD: got %v, want ErrBranchMoved (the human commit is real drift)", err)
	}

	got, err := os.ReadFile(external)
	if err != nil {
		t.Fatalf("ReadFile external: %v", err)
	}
	if string(got) != "untouched" {
		t.Errorf("external file after push: got %q, want %q; the recheck fetch wrote FETCH_HEAD through the symlink", got, "untouched")
	}
}

// checkoutTestBranch creates the shared test PR branch in the origin repo
// pointing at sha and checks it out, so a subsequent commit helper lands on
// that branch rather than the repo's default branch.
func checkoutTestBranch(t *testing.T, repoDir, sha string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	ref := plumbing.NewBranchReferenceName("clonemanager/pr-branch")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, plumbing.NewHash(sha))); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: ref}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
}

// checkoutBranch checks out an existing branch in the origin repo.
func checkoutBranch(t *testing.T, repoDir, branchName string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branchName)}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
}

// commitToTestRepoAs commits name to the origin repo's current branch (see
// checkoutTestBranch) with the given author identity, returning the new
// commit's hash.
func commitToTestRepoAs(t *testing.T, repoDir, name, authorName, authorEmail string) plumbing.Hash {
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
	hash, err := wt.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return hash
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

	b.Run("gogit", func(b *testing.B) { benchmarkLease(b, token, WithGoGit()) })
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

func TestDefaultBackend(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := t.Context()

	t.Run("git on PATH selects the CLI backend", func(t *testing.T) {
		m, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, ok := m.backend.(cliBackend); !ok {
			t.Errorf("backend: got = %T, want = cliBackend", m.backend)
		}
	})

	t.Run("no git falls back to go-git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		m, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, ok := m.backend.(gogitBackend); !ok {
			t.Errorf("backend: got = %T, want = gogitBackend", m.backend)
		}
	})

	t.Run("WithGitCLI without git fails", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, WithGitCLI()); err == nil {
			t.Error("New: got = nil error, want = missing git error")
		}
	})

	t.Run("WithGoGit ignores git on PATH", func(t *testing.T) {
		m, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, WithGoGit())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, ok := m.backend.(gogitBackend); !ok {
			t.Errorf("backend: got = %T, want = gogitBackend", m.backend)
		}
	})
}

// plantRepository initializes a git repository at dir, as untrusted code with
// worktree write access could, with one file beside its .git.
func plantRepository(t *testing.T, dir string) {
	t.Helper()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.yaml"), []byte("planted"), 0o644); err != nil {
		t.Fatalf("WriteFile payload: %v", err)
	}
}

// plantGitfile writes a gitfile at dir/.git pointing elsewhere, the other form
// a nested repository's marker takes, with one file beside it.
func plantGitfile(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+t.TempDir()+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile gitfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.yaml"), []byte("planted"), 0o644); err != nil {
		t.Fatalf("WriteFile payload: %v", err)
	}
}

func TestReturnDiscardsNestedRepository(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, dir string)
	}{
		{"repository", plantRepository},
		{"gitfile", plantGitfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, opts ...Option) {
				testReturnDiscardsNestedRepository(t, tc.plant, opts...)
			})
		})
	}
}

func testReturnDiscardsNestedRepository(t *testing.T, plant func(t *testing.T, dir string), opts ...Option) {
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

	// Lease 1 plants a repository in the tracked packages directory, which
	// no clean removes, and returns the clone.
	lease1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	dir := lease1.WorkingTree()
	plant(t, filepath.Join(dir, "packages"))
	if err := lease1.Return(ctx); err == nil {
		t.Error("Return 1: got = nil, want = an error naming the nested repository")
	}

	// Lease 2 gets a fresh clone without the planted repository or its file.
	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 2: %v", err)
	}
	t.Cleanup(func() { _ = lease2.Return(ctx) })
	if lease2.WorkingTree() == dir {
		t.Errorf("Lease 2 working tree: got = %s, want = a fresh clone", lease2.WorkingTree())
	}
	for _, p := range []string{filepath.Join("packages", ".git"), filepath.Join("packages", "payload.yaml")} {
		if _, err := os.Lstat(filepath.Join(lease2.WorkingTree(), p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s in Lease 2: got = %v, want = %v", p, err, os.ErrNotExist)
		}
	}
}

func TestReturnCleansUntrackedNestedRepository(t *testing.T) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, WithGitCLI())
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

	// A repository in an untracked directory, such as a tool's cache, is
	// cleaned away, so the clone stays pooled.
	lease1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	dir := lease1.WorkingTree()
	plantRepository(t, filepath.Join(dir, "cache", "module"))
	if err := lease1.Return(ctx); err != nil {
		t.Fatalf("Return 1: %v", err)
	}

	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 2: %v", err)
	}
	t.Cleanup(func() { _ = lease2.Return(ctx) })
	if lease2.WorkingTree() != dir {
		t.Fatalf("Lease 2 working tree: got = %s, want = %s", lease2.WorkingTree(), dir)
	}
	if _, err := os.Lstat(filepath.Join(dir, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache after Return: got = %v, want = %v", err, os.ErrNotExist)
	}
}

// The go-git backend's clean keeps a repository in an untracked directory, so
// the clone holding it is discarded instead.
func TestReturnDiscardsUntrackedNestedRepositoryWithGoGit(t *testing.T) {
	ctx := t.Context()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, WithGoGit())
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

	lease1, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 1: %v", err)
	}
	dir := lease1.WorkingTree()
	plantRepository(t, filepath.Join(dir, "cache", "module"))
	if err := lease1.Return(ctx); err == nil {
		t.Error("Return 1: got = nil, want = an error naming the nested repository")
	}

	lease2, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease 2: %v", err)
	}
	t.Cleanup(func() { _ = lease2.Return(ctx) })
	if lease2.WorkingTree() == dir {
		t.Errorf("Lease 2 working tree: got = %s, want = a fresh clone", lease2.WorkingTree())
	}
	if _, err := os.Lstat(filepath.Join(lease2.WorkingTree(), "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache in Lease 2: got = %v, want = %v", err, os.ErrNotExist)
	}
}

func TestNestedRepositoryStopsWithContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := nestedRepository(ctx, root); !errors.Is(err, context.Canceled) {
		t.Errorf("nestedRepository with a done context: got = %v, want = %v", err, context.Canceled)
	}
}
