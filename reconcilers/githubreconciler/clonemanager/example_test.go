/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/reconcilers/githubreconciler"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v88/github"
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

// ExampleWithGitCLI constructs a Manager whose clones and fetches shell out
// to the git binary. The lease API is unchanged: only the transport differs,
// trading go-git's full-snapshot shallow fetches for native git's negotiated
// incremental packs.
func ExampleWithGitCLI() {
	ctx := context.Background()

	repoDir := initExampleRepo()

	repoURL = func(*githubreconciler.Resource) string { return repoDir }
	defer func() { repoURL = defaultRemoteURL }()

	mgr, err := New(ctx, staticTokenSource(""), "automation", nil, WithGitCLI())
	if err != nil {
		fmt.Println("error creating manager:", err)
		return
	}

	lease, err := mgr.Lease(ctx, &githubreconciler.Resource{
		Owner: "example",
		Repo:  repoDir,
		Ref:   "master",
		Path:  "packages/example.yaml",
		Type:  githubreconciler.ResourceTypePath,
	})
	if err != nil {
		fmt.Println("lease error:", err)
		return
	}

	fmt.Println("path exists:", lease.PathExists())

	if err := lease.Return(ctx); err != nil {
		fmt.Println("return error:", err)
		return
	}

	// Output:
	// path exists: true
}

// ExampleWorktreeCallbacks demonstrates using WorktreeCallbacks for AI agent integration.
// WorktreeCallbacks creates WorktreeTools from a git worktree, which can be passed
// to an AI agent (via metaagent.BaseCallbacks) to allow it to read, write, search,
// move, and copy files. Writes go to the worktree on disk; the changes are
// staged in one shot at commit time, not per callback.
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
		// - Read files with offset/limit windowing
		// - Write files to the worktree (staged once at commit time)
		// - Delete, move, copy files
		// - List directory contents with metadata and filtering
		// - Search for patterns across the codebase
		wtTools := WorktreeCallbacks(wt)

		// Example: Read a file using the tool callback (offset=0, limit=-1 for full read)
		result, err := wtTools.ReadFile(ctx, "packages/example.yaml", 0, -1)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		fmt.Println("file content:", result.Content)

		// Example: Write a file (staged at commit time)
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

// ExampleFetchFile reads a single file over the GitHub REST API using the client
// the reconciler was handed. Unlike Lease, it does not clone, so a reconciler
// that only reads one path avoids the tree transfer. A path that is absent at the
// ref reports ErrFileNotFound, which callers typically treat as a normal state
// rather than a failure.
func ExampleFetchFile() {
	ctx := context.Background()

	// Stand in for api.github.com: serve one file and 404 everything else.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/contents/packages/example.yaml") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","path":"packages/example.yaml","sha":"a1b2c3","content":%q}`,
			base64.StdEncoding.EncodeToString([]byte("name: example\n")))
	}))
	defer srv.Close()

	// In a reconciler this is the *github.Client passed to the ReconcilerFunc.
	gh, err := github.NewClient(github.WithEnterpriseURLs(srv.URL+"/api/v3/", srv.URL+"/api/v3/"))
	if err != nil {
		fmt.Println("error creating client:", err)
		return
	}

	res := &githubreconciler.Resource{
		Owner: "example",
		Repo:  "repo",
		Type:  githubreconciler.ResourceTypePath,
		Ref:   "main",
		Path:  "packages/example.yaml",
	}

	file, err := FetchFile(ctx, gh, res)
	if err != nil {
		fmt.Println("fetch error:", err)
		return
	}
	fmt.Printf("%s at %s: %s", file.Path, file.SHA, file.Content)

	res.Path = "packages/missing.yaml"
	_, err = FetchFile(ctx, gh, res)
	fmt.Println("missing file not found:", errors.Is(err, ErrFileNotFound))

	// Output:
	// packages/example.yaml at a1b2c3: name: example
	// missing file not found: true
}

// ExampleFetchFileRef reads res.Path at a ref of the caller's choosing rather
// than res.Ref. A reconciler that has to compare a file across refs -- the merge
// base of a pull request against its head, say -- uses this instead of mutating
// the resource it was handed.
func ExampleFetchFileRef() {
	ctx := context.Background()

	// Stand in for api.github.com: serve content that depends on ?ref=.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := "name: example\nversion: 2\n"
		if r.URL.Query().Get("ref") == "v1.0.0" {
			body = "name: example\nversion: 1\n"
		}
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","path":"packages/example.yaml","sha":"a1b2c3","content":%q}`,
			base64.StdEncoding.EncodeToString([]byte(body)))
	}))
	defer srv.Close()

	gh, err := github.NewClient(github.WithEnterpriseURLs(srv.URL+"/api/v3/", srv.URL+"/api/v3/"))
	if err != nil {
		fmt.Println("error creating client:", err)
		return
	}

	res := &githubreconciler.Resource{
		Owner: "example",
		Repo:  "repo",
		Type:  githubreconciler.ResourceTypePath,
		Ref:   "main",
		Path:  "packages/example.yaml",
	}

	// The tag, not res.Ref.
	tagged, err := FetchFileRef(ctx, gh, res, "v1.0.0")
	if err != nil {
		fmt.Println("fetch error:", err)
		return
	}
	fmt.Printf("at v1.0.0:\n%s", tagged.Content)

	// res.Ref is untouched, so FetchFile still reads main.
	head, err := FetchFile(ctx, gh, res)
	if err != nil {
		fmt.Println("fetch error:", err)
		return
	}
	fmt.Printf("at %s:\n%s", res.Ref, head.Content)

	// Output:
	// at v1.0.0:
	// name: example
	// version: 1
	// at main:
	// name: example
	// version: 2
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
