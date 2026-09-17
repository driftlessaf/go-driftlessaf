/*
Copyright 2026 Chainguard, Inc.
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

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestMakeAndPushChangesCommitScopeGuard proves the opt-in guard end to end: a
// file written through WorktreeCallbacks and a modification to a tracked file are
// committed, while artifacts dropped into the worktree by other means are left
// out of the pushed commit.
func TestMakeAndPushChangesCommitScopeGuard(t *testing.T) {
	forEachBackend(t, testCommitScopeGuard)
}

func testCommitScopeGuard(t *testing.T, opts ...Option) {
	// Isolate the developer's git configuration so a global excludesfile cannot
	// change which paths appear as untracked.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ctx := context.Background()

	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, append(opts, WithCommitScopeGuard(commitscope.DefaultDenylist()))...)
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

	binaryLitter := string([]byte{0x7f, 'E', 'L', 'F', 0x00, 0x01})
	branchName := "clonemanager/guard-branch"

	if err := lease.MakeAndPushChanges(ctx, branchName, func(ctx context.Context, wt *git.Worktree) (string, error) {
		cb := WorktreeCallbacks(wt)
		root := wt.Filesystem.Root()

		// An edit tool writes a new source file: recorded in the scope carried by
		// ctx, so the guard treats it as intended.
		if err := cb.WriteFile(ctx, "packages/new.go", "package fresh\n", 0o644); err != nil {
			return "", fmt.Errorf("WriteFile new.go: %w", err)
		}

		// A finalizer rewrites a tracked file in place (not recorded): kept via
		// the tracked-modification proxy.
		if err := os.WriteFile(filepath.Join(root, "packages", "foo.yaml"), []byte("name: foo-updated"), 0o644); err != nil {
			return "", fmt.Errorf("write foo.yaml: %w", err)
		}

		// Gates litter the tree. None are recorded and each is either denylisted
		// or unintended.
		if err := os.MkdirAll(filepath.Join(root, "infra", ".terraform"), 0o755); err != nil {
			return "", fmt.Errorf("mkdir .terraform: %w", err)
		}
		litter := map[string]string{
			filepath.Join("infra", ".terraform", "provider"): "binary",
			"coverage.out": "mode: set\n",
			"app.test":     binaryLitter,
			"scratch.txt":  "unintended scratch\n",
		}
		for rel, content := range litter {
			if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("write litter %q: %w", rel, err)
			}
		}
		return "guarded change", nil
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
	pushed, err := originRepo.CommitObject(branchRef.Hash())
	if err != nil {
		t.Fatalf("load pushed commit: %v", err)
	}

	// A net-new source file written through the edit tools is committed, and so
	// is an in-place modification of a tracked file.
	wantPresent := map[string]string{
		"packages/new.go":   "package fresh\n",
		"packages/foo.yaml": "name: foo-updated",
	}
	for path, want := range wantPresent {
		f, err := pushed.File(path)
		if err != nil {
			t.Fatalf("pushed commit missing %q: %v", path, err)
		}
		got, err := f.Contents()
		if err != nil {
			t.Fatalf("read %q: %v", path, err)
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", path, got, want)
		}
	}

	for _, path := range []string{"infra/.terraform/provider", "coverage.out", "app.test", "scratch.txt"} {
		if _, err := pushed.File(path); err == nil {
			t.Errorf("pushed commit unexpectedly contains artifact %q", path)
		}
	}
}

// TestMakeAndPushChangesGuardAllArtifactsIsNothingToCommit proves a run whose
// only worktree changes are artifacts produces no commit rather than an error.
func TestMakeAndPushChangesGuardAllArtifactsIsNothingToCommit(t *testing.T) {
	forEachBackend(t, testGuardAllArtifactsIsNothingToCommit)
}

func testGuardAllArtifactsIsNothingToCommit(t *testing.T, opts ...Option) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ctx := context.Background()
	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, append(opts, WithCommitScopeGuard(commitscope.DefaultDenylist()))...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	repoDir, _ := initTestRepo(t)
	res := &githubreconciler.Resource{
		Owner: "tests", Repo: repoDir, Ref: "master",
		Path: "packages/foo.yaml", Type: githubreconciler.ResourceTypePath,
	}
	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	t.Cleanup(func() { repoURL = defaultRemoteURL })

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	err = lease.MakeAndPushChanges(ctx, "clonemanager/guard-empty", func(ctx context.Context, wt *git.Worktree) (string, error) {
		cb := WorktreeCallbacks(wt)
		// The agent writes only a denylisted artifact.
		if err := cb.WriteFile(ctx, "app.test", "\x00binary", 0o644); err != nil {
			return "", err
		}
		return "artifacts only", nil
	})
	if !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("MakeAndPushChanges: got err = %v, want ErrNothingToCommit", err)
	}
	if err := lease.Return(ctx); err != nil {
		t.Fatalf("Return: %v", err)
	}
}
