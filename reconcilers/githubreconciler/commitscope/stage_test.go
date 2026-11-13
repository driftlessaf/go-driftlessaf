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

	gogit "github.com/go-git/go-git/v5"
)

func TestStageIntentAndDenylist(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{
		"go.sum":     "old\n",
		"keep.go":    "package keep\n",
		"remove.go":  "package remove\n",
		"assets.bin": binaryContent, // tracked binary at base
	})
	commitAll(t, wt)
	base := headTree(t, repo)

	// Agent edits, recorded in scope.
	writeFileAt(t, root, "new.go", "package fresh\n")
	if err := os.Remove(filepath.Join(root, "remove.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFileAt(t, root, "assets.bin", binaryContent+"\x00more") // tracked binary modified

	// Finalizer edit, not in scope; caught by the tracked-modification proxy.
	writeFileAt(t, root, "go.sum", "new sum\n")

	// Litter from gates, not intended.
	writeFileAt(t, root, "cmd/tool/tool", binaryContent)             // untracked binary
	writeFileAt(t, root, "infra/.terraform/providers/x", "provider") // artifact dir
	writeFileAt(t, root, "coverage.out", "mode: set\n")              // artifact file
	writeFileAt(t, root, "notes.txt", "scratch notes\n")             // unintended untracked text

	scope := NewScope()
	scope.Touch("new.go", "remove.go", "assets.bin")

	res, err := Stage(t.Context(), wt, base, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if _, err := wt.Commit("guarded", &gogit.CommitOptions{Author: testSig(), Committer: testSig()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := committedPaths(t, repo)
	want := []string{"assets.bin", "go.sum", "keep.go", "new.go"} // remove.go deleted
	if slices.Compare(got, want) != 0 {
		t.Errorf("committed paths: got = %v, want = %v", got, want)
	}

	dropped := droppedPaths(res)
	for _, p := range []string{"cmd/tool/tool", "coverage.out", "infra/.terraform/providers/x", "notes.txt"} {
		if !slices.Contains(dropped, p) {
			t.Errorf("dropped: got = %v, want to contain %q", dropped, p)
		}
	}
	if c := readCommitted(t, repo, "go.sum"); c != "new sum\n" {
		t.Errorf("go.sum content: got = %q, want = %q", c, "new sum\n")
	}
}

func TestStageSymlinkReplacedByFileRefused(t *testing.T) {
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
	// Base: a symlinked lock file plus its real target.
	writeFileAt(t, root, "shared.lock", "provider hashes\n")
	if err := os.Symlink("shared.lock", filepath.Join(root, ".terraform.lock.hcl")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	commitAll(t, wt)
	base := headTree(t, repo)

	// A tool replaces the symlink with a regular file.
	if err := os.Remove(filepath.Join(root, ".terraform.lock.hcl")); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	writeFileAt(t, root, ".terraform.lock.hcl", "rewritten by init\n")

	scope := NewScope()
	scope.Touch(".terraform.lock.hcl") // even when the agent touched it

	res, err := Stage(t.Context(), wt, base, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(res.Staged) != 0 {
		t.Errorf("staged a de-symlinked lock file: %v", stagedPaths(res))
	}
	if !slices.Contains(droppedPaths(res), ".terraform.lock.hcl") {
		t.Errorf("lock file not dropped: %v", droppedPaths(res))
	}
}

func TestStageOnlyDenylistedYieldsNothing(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)
	base := headTree(t, repo)

	writeFileAt(t, root, "app.test", binaryContent)
	writeFileAt(t, root, "infra/.terraform/x", "provider")

	scope := NewScope()
	scope.Touch("app.test", "infra/.terraform/x") // agent-written, still denied

	res, err := Stage(t.Context(), wt, base, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(res.Staged) != 0 {
		t.Errorf("staged denylisted-only changes: %v", stagedPaths(res))
	}
}

func TestStageLockUpdateDeclared(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{".terraform.lock.hcl": "h1:old\n"})
	commitAll(t, wt)
	base := headTree(t, repo)

	writeFileAt(t, root, ".terraform.lock.hcl", "h1:new\n")
	scope := NewScope()
	scope.Touch(".terraform.lock.hcl")
	scope.DeclareLockUpdate()

	res, err := Stage(t.Context(), wt, base, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !slices.Contains(stagedPaths(res), ".terraform.lock.hcl") {
		t.Errorf("declared lock update not staged: %v", stagedPaths(res))
	}
}

// TestConfined proves the fail-closed boundary: a status path that cleans to the
// worktree root or escapes it is rejected, so Stage never asks go-git to add "."
// (which would stage the whole tree).
func TestConfined(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantOut string
	}{
		{name: "plain path", raw: "pkg/a.go", wantOK: true, wantOut: "pkg/a.go"},
		{name: "dot-relative cleaned", raw: "./pkg/a.go", wantOK: true, wantOut: "pkg/a.go"},
		{name: "interior dotdot cleaned within tree", raw: "pkg/sub/../a.go", wantOK: true, wantOut: "pkg/a.go"},
		{name: "dot is worktree root", raw: ".", wantOK: false},
		{name: "empty", raw: "", wantOK: false},
		{name: "leading dotdot escapes", raw: "../evil", wantOK: false},
		{name: "cleans to escape", raw: "a/../../evil", wantOK: false},
		{name: "absolute rejected", raw: "/etc/passwd", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := confined(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("confined(%q): ok = %v, want %v (got = %q)", tc.raw, ok, tc.wantOK, got)
			}
			if ok && got != tc.wantOut {
				t.Errorf("confined(%q): got = %q, want %q", tc.raw, got, tc.wantOut)
			}
		})
	}
}

// TestStageDeletesArtifactNamedTrackedFile proves an intended deletion of a
// tracked file whose name matches an artifact pattern is staged, so the deletion
// lands in the commit rather than being silently dropped by the denylist.
func TestStageDeletesArtifactNamedTrackedFile(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{
		"keep.go":      "package keep\n",
		"logs/run.log": "stale tracked log\n",
	})
	commitAll(t, wt)
	base := headTree(t, repo)

	if err := os.Remove(filepath.Join(root, "logs", "run.log")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	scope := NewScope()
	scope.Touch("logs/run.log") // the agent intended the deletion

	res, err := Stage(t.Context(), wt, base, scope, DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !slices.Contains(stagedPaths(res), "logs/run.log") {
		t.Errorf("intended deletion not staged: staged = %v, dropped = %v", stagedPaths(res), droppedPaths(res))
	}
	if _, err := wt.Commit("delete stale log", &gogit.CommitOptions{Author: testSig(), Committer: testSig()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := committedPaths(t, repo); slices.Contains(got, "logs/run.log") {
		t.Errorf("deleted artifact-named file still committed: %v", got)
	}
}

func TestStageInactiveScopeUsesProxyOnly(t *testing.T) {
	repo, wt, root := initRepo(t, map[string]string{"keep.go": "package keep\n"})
	commitAll(t, wt)
	base := headTree(t, repo)

	// Only a tracked modification and an untracked file, no scope entries.
	writeFileAt(t, root, "keep.go", "package keep // edited\n")
	writeFileAt(t, root, "brand-new.go", "package fresh\n")

	res, err := Stage(t.Context(), wt, base, NewScope(), DefaultDenylist())
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// The tracked modification is intended via the proxy; the untracked file is
	// not, because no edit tool recorded it.
	if got := stagedPaths(res); slices.Compare(got, []string{"keep.go"}) != 0 {
		t.Errorf("staged: got = %v, want = [keep.go]", got)
	}
	if !slices.Contains(droppedPaths(res), "brand-new.go") {
		t.Errorf("unrecorded new file not dropped: %v", droppedPaths(res))
	}
}
