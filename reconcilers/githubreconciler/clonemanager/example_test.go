/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func ExampleLease_MakeAndPushChanges() {
	ctx := context.Background()

	repoDir := initExampleRepo()

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	defer func() { repoURL = defaultRemoteURL }()

	mgr, err := New(ctx, staticTokenSource(""), "automation", nil)
	if err != nil {
		fmt.Println("error creating manager:", err)
		return
	}

	res := &githubreconciler.Resource{
		Owner: "example",
		Repo:  repoDir,
		Ref:   "master",
		Path:  "packages/example.yaml",
		Type:  githubreconciler.ResourceTypePath,
	}

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		fmt.Println("lease error:", err)
		return
	}

	if err := lease.MakeAndPushChanges(ctx, "automation/example-update", func(_ context.Context, wt *git.Worktree) (string, error) {
		relPath := filepath.ToSlash("packages/example.yaml")
		absPath := filepath.Join(wt.Filesystem.Root(), "packages", "example.yaml")
		if err := os.WriteFile(absPath, []byte("name: example\n"), 0o644); err != nil {
			return "", err
		}
		if _, err := wt.Add(relPath); err != nil {
			return "", err
		}
		return "automation: update example", nil
	}); err != nil {
		fmt.Println("apply error:", err)
		return
	}

	if err := lease.Return(ctx); err != nil {
		fmt.Println("return error:", err)
		return
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		fmt.Println("open origin error:", err)
		return
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("automation/example-update"), true)
	fmt.Println("branch exists:", err == nil)
	fmt.Println("commit author:", ref != nil)

	// Output:
	// branch exists: true
	// commit author: true
}

// ExampleWorktreeCallbacks demonstrates using WorktreeCallbacks for AI agent integration.
// WorktreeCallbacks creates WorktreeTools from a git worktree, which can be passed
// to an AI agent (via metaagent.BaseCallbacks) to allow it to read, write, and search
// files while automatically staging changes.
func ExampleWorktreeCallbacks() {
	ctx := context.Background()

	repoDir := initExampleRepo()

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	defer func() { repoURL = defaultRemoteURL }()

	mgr, err := New(ctx, staticTokenSource(""), "automation", nil)
	if err != nil {
		fmt.Println("error creating manager:", err)
		return
	}

	res := &githubreconciler.Resource{
		Owner: "example",
		Repo:  repoDir,
		Ref:   "master",
		Path:  "packages/example.yaml",
		Type:  githubreconciler.ResourceTypePath,
	}

	lease, err := mgr.Lease(ctx, res)
	if err != nil {
		fmt.Println("lease error:", err)
		return
	}
	defer lease.Return(ctx) //nolint:errcheck

	if err := lease.MakeAndPushChanges(ctx, "automation/agent-update", func(ctx context.Context, wt *git.Worktree) (string, error) {
		// Create WorktreeTools for the agent
		// This provides callbacks that:
		// - Read files from the worktree
		// - Write files and automatically stage changes
		// - Delete files and automatically stage deletions
		// - List directory contents
		// - Search for patterns across the codebase
		wtTools := WorktreeCallbacks(wt)

		// Example: Read a file using the tool callback
		content, err := wtTools.ReadFile(ctx, "packages/example.yaml")
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		fmt.Println("file content:", content)

		// Example: Write a file (automatically staged)
		if err := wtTools.WriteFile(ctx, "packages/example.yaml", "name: updated\n", 0o644); err != nil {
			return "", fmt.Errorf("write file: %w", err)
		}

		return "automation: update via agent", nil
	}); err != nil {
		fmt.Println("apply error:", err)
		return
	}

	fmt.Println("changes pushed successfully")

	// Output:
	// file content: name: example
	// changes pushed successfully
}

func initExampleRepo() string {
	dir, _ := os.MkdirTemp("", "clonemanager-example-")
	repo, _ := git.PlainInit(dir, false)
	wt, _ := repo.Worktree()

	pkgDir := filepath.Join(dir, "packages")
	_ = os.MkdirAll(pkgDir, 0o755)
	absPath := filepath.Join(pkgDir, "example.yaml")
	_ = os.WriteFile(absPath, []byte("name: example"), 0o644)
	_, _ = wt.Add("packages/example.yaml")
	_, _ = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Example", Email: "example@clonemanager", When: time.Now()},
	})
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("master"))); err != nil {
		panic(err)
	}
	return dir
}
