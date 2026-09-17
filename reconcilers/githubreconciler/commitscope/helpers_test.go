/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package commitscope

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// hermeticGit isolates the process from the developer's git configuration for
// the duration of a test: go-git reads the global config, the global
// excludesfile, and the XDG ignore file while computing worktree status, so a
// developer's global gitignore could otherwise hide the untracked litter these
// tests assert on. Pointing HOME and XDG at empty temp dirs, and the git config
// env vars at the null device, makes every global lookup find nothing. The tests
// pass an explicit author and no signer, so no identity or signing key is read.
func hermeticGit(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func testSig() *object.Signature {
	return &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0).UTC()}
}

// initRepo builds a repository whose first commit holds the given regular files,
// and returns the repo, its worktree, and the root path. It calls hermeticGit so
// no developer configuration reaches go-git.
func initRepo(t *testing.T, files map[string]string) (*gogit.Repository, *gogit.Worktree, string) {
	t.Helper()
	hermeticGit(t)
	root := t.TempDir()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for name, content := range files {
		writeFileAt(t, root, name, content)
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}
	return repo, wt, root
}

func writeFileAt(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

func commitAll(t *testing.T, wt *gogit.Worktree) {
	t.Helper()
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	if _, err := wt.Commit("commit", &gogit.CommitOptions{Author: testSig(), Committer: testSig()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func headTree(t *testing.T, repo *gogit.Repository) *object.Tree {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	c, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	tree, err := c.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	return tree
}

// committedPaths returns the sorted paths in HEAD's tree.
func committedPaths(t *testing.T, repo *gogit.Repository) []string {
	t.Helper()
	var paths []string
	if err := headTree(t, repo).Files().ForEach(func(f *object.File) error {
		paths = append(paths, f.Name)
		return nil
	}); err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	slices.Sort(paths)
	return paths
}

func readCommitted(t *testing.T, repo *gogit.Repository, name string) string {
	t.Helper()
	f, err := headTree(t, repo).File(name)
	if err != nil {
		t.Fatalf("tree File(%q): %v", name, err)
	}
	s, err := f.Contents()
	if err != nil {
		t.Fatalf("Contents(%q): %v", name, err)
	}
	return s
}

// binaryContent is a short byte string with an ELF magic and a NUL, so the sniff
// classifies it as binary.
var binaryContent = string([]byte{'\x7f', 'E', 'L', 'F', 0x00, 0x01, 0x02})
