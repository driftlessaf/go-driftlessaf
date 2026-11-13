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
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"chainguard.dev/driftlessaf/reconcilers/githubreconciler/commitscope"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

// TestMakeAndPushChangesUnguardedWorkflowEditRefused proves the guard fails
// closed on the whole-tree fallback: with DenyWorkflowEdits set and no active
// scope (the run wrote around the edit-tool layer), a changed workflow refuses
// the commit, a declared workflow edit lets it through, a run with no workflow
// change still commits through the fallback, and so does one that only deletes
// a tracked workflow, since a deletion runs nothing.
func TestMakeAndPushChangesUnguardedWorkflowEditRefused(t *testing.T) {
	forEachBackend(t, testUnguardedWorkflowEditRefused)
}

func testUnguardedWorkflowEditRefused(t *testing.T, opts ...Option) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := t.Context()

	dl := commitscope.DefaultDenylist()
	dl.DenyWorkflowEdits = true
	mgr, err := New(ctx, staticTokenSource(""), "clonemanager-test", nil, append(opts, WithCommitScopeGuard(dl))...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	writeAround := func(root, rel, content string) error {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(content), 0o644)
	}

	tests := []struct {
		name     string
		branch   string
		workflow bool // the run writes .github/workflows/ci.yaml
		remove   bool // the base tracks .github/workflows/old.yaml and the run deletes it
		restore  bool // with remove: the deletion is staged, then the file is written back
		declare  bool
		wantErr  error
	}{
		{name: "workflow change, no declaration", branch: "clonemanager/unguarded-refused", workflow: true, wantErr: ErrUnguardedWorkflowEdit},
		{name: "workflow change, declared", branch: "clonemanager/unguarded-declared", workflow: true, declare: true},
		{name: "no workflow change", branch: "clonemanager/unguarded-plain"},
		{name: "workflow deletion, no declaration", branch: "clonemanager/unguarded-deletion", remove: true},
		// The index says deleted but the file is back on disk, so the whole-tree
		// add would commit its content: refused like any other workflow change.
		{name: "staged deletion with the file back on disk", branch: "clonemanager/unguarded-restored", remove: true, restore: true, wantErr: ErrUnguardedWorkflowEdit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var repoDir string
			if tc.remove {
				repoDir = initTestRepoWithWorkflow(t)
			} else {
				repoDir, _ = initTestRepo(t)
			}
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
			err = lease.MakeAndPushChanges(ctx, tc.branch, func(ctx context.Context, wt *git.Worktree) (string, error) {
				// Nothing goes through WorktreeCallbacks, so the scope stays
				// inactive and staging takes the whole-tree fallback.
				if tc.declare {
					scope, ok := commitscope.ScopeFromContext(ctx)
					if !ok {
						return "", errors.New("no commit scope in ctx")
					}
					scope.DeclareWorkflowEdit()
				}
				root := wt.Filesystem.Root()
				if tc.workflow {
					if err := writeAround(root, ".github/workflows/ci.yaml", "on: pull_request\n"); err != nil {
						return "", err
					}
				}
				if tc.remove {
					if err := os.Remove(filepath.Join(root, ".github", "workflows", "old.yaml")); err != nil {
						return "", err
					}
				}
				if tc.restore {
					if _, err := wt.Remove(".github/workflows/old.yaml"); err != nil {
						return "", fmt.Errorf("stage deletion: %w", err)
					}
					if err := writeAround(root, ".github/workflows/old.yaml", "on: pull_request\n"); err != nil {
						return "", err
					}
				}
				if err := writeAround(root, "packages/foo.yaml", "name: "+tc.branch); err != nil {
					return "", err
				}
				return "unguarded change", nil
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MakeAndPushChanges: got err = %v, want %v", err, tc.wantErr)
			}
			if err := lease.Return(ctx); err != nil {
				t.Fatalf("Return: %v", err)
			}

			originRepo, err := git.PlainOpen(repoDir)
			if err != nil {
				t.Fatalf("PlainOpen origin: %v", err)
			}
			branchRef, err := originRepo.Reference(plumbing.NewBranchReferenceName(tc.branch), true)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("refused commit was pushed anyway as %s", branchRef.Hash())
				}
				return
			}
			if err != nil {
				t.Fatalf("Reference lookup: %v", err)
			}
			pushed, err := originRepo.CommitObject(branchRef.Hash())
			if err != nil {
				t.Fatalf("load pushed commit: %v", err)
			}
			_, err = pushed.File(".github/workflows/ci.yaml")
			if tc.workflow && err != nil {
				t.Errorf("pushed commit is missing the declared workflow edit: %v", err)
			}
			if !tc.workflow && err == nil {
				t.Errorf("pushed commit unexpectedly contains a workflow")
			}
			if tc.remove {
				if _, err := pushed.File(".github/workflows/old.yaml"); err == nil {
					t.Errorf("pushed commit still contains the deleted workflow")
				}
			}
		})
	}
}

// initTestRepoWithWorkflow is initTestRepo with a workflow tracked in the base
// commit, so a run can delete a tracked workflow. The backends clone at depth
// 1, so the workflow has to be in the initial commit rather than a second one.
func initTestRepoWithWorkflow(t *testing.T) string {
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
	for rel, content := range map[string]string{
		"packages/foo.yaml":          "name: foo",
		".github/workflows/old.yaml": "on: push\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("Add %s: %v", rel, err)
		}
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	return dir
}
