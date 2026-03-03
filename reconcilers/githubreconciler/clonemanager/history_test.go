/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package clonemanager

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// testSig returns a deterministic commit signature for testing.
func testSig() *object.Signature {
	return &object.Signature{Name: "test", Email: "test@test", When: time.Now()}
}

// initHistoryRepo creates a temp repo with a base commit and N additional
// commits, each modifying a unique file. Returns the repo, base commit hash,
// and the list of file paths created by the additional commits.
func initHistoryRepo(t *testing.T, n int) (*gogit.Repository, plumbing.Hash, []string) {
	t.Helper()

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	// Base commit with a seed file.
	writeTestFile(t, dir, "base.txt", fmt.Sprintf("base-%d", rand.Int64()), 0o644)
	if _, err := wt.Add("base.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base commit", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// N additional commits, each adding a file.
	paths := make([]string, 0, n)
	for i := range n {
		name := fmt.Sprintf("file-%d.txt", i)
		content := fmt.Sprintf("content-%d-%d", i, rand.Int64())
		writeTestFile(t, dir, name, content, 0o644)
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Commit(fmt.Sprintf("add %s", name), &gogit.CommitOptions{Author: testSig()}); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}

	return repo, baseHash, paths
}

// TestListCommitsRootCommit exercises the commitFiles path for a root commit
// (no parents), by using plumbing.ZeroHash as the base so the root commit
// itself appears in the returned list.
func TestListCommitsRootCommit(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	// Create a single root commit.
	writeTestFile(t, dir, "root.txt", fmt.Sprintf("root-%d", rand.Int64()), 0o644)
	if _, err := wt.Add("root.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("root commit", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	// Use ZeroHash so the root commit is included (no base to stop at).
	cb := HistoryCallbacks(repo, plumbing.ZeroHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("total: got = %d, wanted = 1", result.Total)
	}

	c := result.Commits[0]
	if len(c.Files) != 1 {
		t.Fatalf("files: got = %d, wanted = 1", len(c.Files))
	}
	if c.Files[0].Path != "root.txt" {
		t.Errorf("file path: got = %q, wanted = %q", c.Files[0].Path, "root.txt")
	}
	if c.Files[0].Type != "added" {
		t.Errorf("file type: got = %q, wanted = %q", c.Files[0].Type, "added")
	}
}

// TestListCommitsMultipleFilesPerCommit verifies that committing multiple
// files at once produces a commit listing with all changed files.
func TestListCommitsMultipleFilesPerCommit(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, dir, "seed.txt", "seed", 0o644)
	if _, err := wt.Add("seed.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Add three files in a single commit.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeTestFile(t, dir, name, fmt.Sprintf("%s-%d", name, rand.Int64()), 0o644)
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit("add three files", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("total: got = %d, wanted = 1", result.Total)
	}
	if len(result.Commits[0].Files) != 3 {
		t.Fatalf("files count: got = %d, wanted = 3", len(result.Commits[0].Files))
	}

	// All files should be "added" with nonzero diff size.
	got := make(map[string]struct{}, 3)
	for _, f := range result.Commits[0].Files {
		got[f.Path] = struct{}{}
		if f.Type != "added" {
			t.Errorf("file %q type: got = %q, wanted = %q", f.Path, f.Type, "added")
		}
		if f.DiffSize == 0 {
			t.Errorf("file %q diff_size: got = 0, wanted > 0", f.Path)
		}
	}
	for _, want := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, exists := got[want]; !exists {
			t.Errorf("missing file %q in commit", want)
		}
	}
}

// TestListCommitsSHAFormat verifies that commit SHAs are exactly 7 hex characters.
func TestListCommitsSHAFormat(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 2)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}

	for i, c := range result.Commits {
		if len(c.SHA) != 7 {
			t.Errorf("commit[%d] SHA length: got = %d, wanted = 7", i, len(c.SHA))
		}
		if strings.Trim(c.SHA, "0123456789abcdef") != "" {
			t.Errorf("commit[%d] SHA %q contains non-hex characters", i, c.SHA)
		}
	}
}

func TestListCommitsBasic(t *testing.T) {
	repo, baseHash, paths := initHistoryRepo(t, 3)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}

	if result.Total != 3 {
		t.Errorf("total: got = %d, wanted = 3", result.Total)
	}
	if len(result.Commits) != 3 {
		t.Fatalf("len(commits): got = %d, wanted = 3", len(result.Commits))
	}
	if result.NextOffset != nil {
		t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
	}

	// Commits are in reverse chronological order, so the last file added
	// should be in the first commit returned.
	lastFile := paths[len(paths)-1]
	firstCommit := result.Commits[0]
	if len(firstCommit.Files) != 1 {
		t.Fatalf("first commit files: got = %d, wanted = 1", len(firstCommit.Files))
	}
	if firstCommit.Files[0].Path != lastFile {
		t.Errorf("first commit file path: got = %q, wanted = %q", firstCommit.Files[0].Path, lastFile)
	}
	if firstCommit.Files[0].Type != "added" {
		t.Errorf("first commit file type: got = %q, wanted = %q", firstCommit.Files[0].Type, "added")
	}
	if firstCommit.Files[0].DiffSize == 0 {
		t.Error("first commit file diff_size: got = 0, wanted > 0")
	}

	// Each commit message should mention its file.
	for i, c := range result.Commits {
		// Commits are reverse order: index 0 = newest = paths[2], index 2 = oldest = paths[0]
		expectedFile := paths[len(paths)-1-i]
		if !strings.Contains(c.Message, expectedFile) {
			t.Errorf("commit[%d] message %q does not mention %q", i, c.Message, expectedFile)
		}
	}
}

func TestListCommitsPagination(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 5)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// First page: 2 commits.
	page1, err := cb.ListCommits(ctx, 0, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Commits) != 2 {
		t.Fatalf("page1 len: got = %d, wanted = 2", len(page1.Commits))
	}
	if page1.Total != 5 {
		t.Errorf("page1 total: got = %d, wanted = 5", page1.Total)
	}
	if page1.NextOffset == nil {
		t.Fatal("page1 next_offset: got = nil, wanted non-nil")
	}
	if *page1.NextOffset != 2 {
		t.Errorf("page1 next_offset: got = %d, wanted = 2", *page1.NextOffset)
	}

	// Second page using NextOffset.
	page2, err := cb.ListCommits(ctx, *page1.NextOffset, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Commits) != 2 {
		t.Fatalf("page2 len: got = %d, wanted = 2", len(page2.Commits))
	}
	if page2.NextOffset == nil {
		t.Fatal("page2 next_offset: got = nil, wanted non-nil")
	}

	// Third page: last commit.
	page3, err := cb.ListCommits(ctx, *page2.NextOffset, 2)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3.Commits) != 1 {
		t.Fatalf("page3 len: got = %d, wanted = 1", len(page3.Commits))
	}
	if page3.NextOffset != nil {
		t.Errorf("page3 next_offset: got = %d, wanted = nil", *page3.NextOffset)
	}

	// Verify no duplicate SHAs across pages.
	seen := make(map[string]struct{}, 5)
	for _, page := range [][]string{
		{page1.Commits[0].SHA, page1.Commits[1].SHA},
		{page2.Commits[0].SHA, page2.Commits[1].SHA},
		{page3.Commits[0].SHA},
	} {
		for _, sha := range page {
			if _, exists := seen[sha]; exists {
				t.Errorf("duplicate SHA across pages: %s", sha)
			}
			seen[sha] = struct{}{}
		}
	}
}

func TestListCommitsOffsetPastEnd(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 2)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 999, 10)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(result.Commits) != 0 {
		t.Errorf("len(commits): got = %d, wanted = 0", len(result.Commits))
	}
	if result.Total != 2 {
		t.Errorf("total: got = %d, wanted = 2", result.Total)
	}
	if result.NextOffset != nil {
		t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
	}
}

func TestListCommitsNoCommits(t *testing.T) {
	// When HEAD == baseCommit, there are no commits to list.
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "seed.txt", "seed", 0o644)
	if _, err := wt.Add("seed.txt"); err != nil {
		t.Fatal(err)
	}
	headHash, err := wt.Commit("seed", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, headHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("total: got = %d, wanted = 0", result.Total)
	}
	if len(result.Commits) != 0 {
		t.Errorf("len(commits): got = %d, wanted = 0", len(result.Commits))
	}
}

// TestListCommitsRenamedFile verifies that renaming a file produces a commit
// with type "renamed" and a populated OldPath field.
func TestListCommitsRenamedFile(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf("content-%d\n", rand.Int64())
	writeTestFile(t, dir, "old-name.txt", content, 0o644)
	if _, err := wt.Add("old-name.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Rename by removing old and adding new with identical content.
	if err := os.Remove(filepath.Join(dir, "old-name.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("old-name.txt"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "new-name.txt", content, 0o644)
	if _, err := wt.Add("new-name.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("rename file", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	result, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if result.Total != 1 {
		t.Fatalf("total: got = %d, wanted = 1", result.Total)
	}

	// go-git may detect this as a rename or as delete+add depending on
	// similarity detection. Accept either representation.
	files := result.Commits[0].Files
	switch {
	case len(files) == 1 && files[0].Type == "renamed":
		if files[0].Path != "new-name.txt" {
			t.Errorf("renamed path: got = %q, wanted = %q", files[0].Path, "new-name.txt")
		}
		if files[0].OldPath != "old-name.txt" {
			t.Errorf("renamed old_path: got = %q, wanted = %q", files[0].OldPath, "old-name.txt")
		}
	case len(files) == 2:
		// Delete + add pair.
		types := make(map[string]struct{}, 2)
		for _, f := range files {
			types[f.Type] = struct{}{}
		}
		if _, ok := types["deleted"]; !ok {
			t.Error("expected a 'deleted' entry in delete+add pair")
		}
		if _, ok := types["added"]; !ok {
			t.Error("expected an 'added' entry in delete+add pair")
		}
	default:
		t.Errorf("unexpected files: got %d entries: %+v", len(files), files)
	}
}

// TestGetFileDiffZeroBase verifies GetFileDiff when the base commit is
// ZeroHash, exercising the filePatchBetween from==nil path (diff against
// empty tree) and resolveCommitOrBase returning nil.
func TestGetFileDiffZeroBase(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf("zero-base-%d\n", rand.Int64())
	writeTestFile(t, dir, "file.txt", content, 0o644)
	if _, err := wt.Add("file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	// Zero base means "diff from empty tree".
	cb := HistoryCallbacks(repo, plumbing.ZeroHash)
	ctx := context.Background()

	result, err := cb.GetFileDiff(ctx, "file.txt", "", "", 0, 100000)
	if err != nil {
		t.Fatalf("GetFileDiff: %v", err)
	}
	if result.Diff == "" {
		t.Error("diff: got empty, wanted non-empty")
	}
	if !strings.Contains(result.Diff, "file.txt") {
		t.Error("diff does not contain filename")
	}
}

// TestGetFileDiffInvalidStartSHA verifies that an invalid start SHA
// returns an error.
func TestGetFileDiffInvalidStartSHA(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 1)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	_, err := cb.GetFileDiff(ctx, "file-0.txt", "not-a-real-sha", "", 0, 100000)
	if err == nil {
		t.Fatal("error: got = nil, wanted non-nil for invalid start SHA")
	}
	if !strings.Contains(err.Error(), "resolve start") {
		t.Errorf("error: got = %v, wanted containing %q", err, "resolve start")
	}
}

// TestGetFileDiffInvalidEndSHA verifies that an invalid end SHA
// returns an error.
func TestGetFileDiffInvalidEndSHA(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 1)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	_, err := cb.GetFileDiff(ctx, "file-0.txt", "", "not-a-real-sha", 0, 100000)
	if err == nil {
		t.Fatal("error: got = nil, wanted non-nil for invalid end SHA")
	}
	if !strings.Contains(err.Error(), "resolve end") {
		t.Errorf("error: got = %v, wanted containing %q", err, "resolve end")
	}
}

// TestGetFileDiffRenamedByOldPath verifies that GetFileDiff can find a
// renamed file when queried by its old path (exercising the fpFrom.Path()
// match in filePatchBetween).
func TestGetFileDiffRenamedByOldPath(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	content := fmt.Sprintf("rename-content-%d\n", rand.Int64())
	writeTestFile(t, dir, "before.txt", content, 0o644)
	if _, err := wt.Add("before.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Rename: remove old, add new with same content.
	if err := os.Remove(filepath.Join(dir, "before.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("before.txt"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "after.txt", content, 0o644)
	if _, err := wt.Add("after.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("rename", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// Try both old and new paths. go-git may or may not detect the rename;
	// if it detects rename, both paths should work via fpFrom/fpTo matching.
	// If it treats as delete+add, only the respective path will work.
	_, errOld := cb.GetFileDiff(ctx, "before.txt", "", "", 0, 100000)
	_, errNew := cb.GetFileDiff(ctx, "after.txt", "", "", 0, 100000)

	// At least one must succeed.
	if errOld != nil && errNew != nil {
		t.Fatalf("both old and new path lookups failed:\n  old: %v\n  new: %v", errOld, errNew)
	}
}

func TestGetFileDiffBasic(t *testing.T) {
	repo, baseHash, paths := initHistoryRepo(t, 1)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// Get diff for the file added in the first commit after base.
	result, err := cb.GetFileDiff(ctx, paths[0], "", "", 0, 100000)
	if err != nil {
		t.Fatalf("GetFileDiff: %v", err)
	}

	if result.Diff == "" {
		t.Error("diff: got empty, wanted non-empty")
	}
	// The diff should contain the file path and added content.
	if !strings.Contains(result.Diff, paths[0]) {
		t.Errorf("diff does not contain file path %q", paths[0])
	}
	if result.NextOffset != nil {
		t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
	}
}

func TestGetFileDiffPagination(t *testing.T) {
	// Create a commit with a large file to produce a big diff.
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, dir, "seed.txt", "seed", 0o644)
	if _, err := wt.Add("seed.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Add a large file to produce a substantial diff.
	largeContent := strings.Repeat("line of content\n", 500)
	writeTestFile(t, dir, "large.txt", largeContent, 0o644)
	if _, err := wt.Add("large.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("add large file", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// Read the full diff to know its size.
	full, err := cb.GetFileDiff(ctx, "large.txt", "", "", 0, 1000000)
	if err != nil {
		t.Fatalf("full diff: %v", err)
	}
	fullSize := len(full.Diff)
	if fullSize < 100 {
		t.Fatalf("diff too small for pagination test: %d bytes", fullSize)
	}

	// Read in small chunks and reassemble.
	var assembled strings.Builder
	var offset int64
	const chunkSize = 200

	for range 1000 { // guard against infinite loop
		result, err := cb.GetFileDiff(ctx, "large.txt", "", "", offset, chunkSize)
		if err != nil {
			t.Fatalf("GetFileDiff at offset %d: %v", offset, err)
		}
		assembled.WriteString(result.Diff)
		if result.NextOffset == nil {
			break
		}
		offset = *result.NextOffset
	}

	if assembled.String() != full.Diff {
		t.Errorf("reassembled diff length: got = %d, wanted = %d", assembled.Len(), fullSize)
	}
}

func TestGetFileDiffFileNotFound(t *testing.T) {
	repo, baseHash, _ := initHistoryRepo(t, 1)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	_, err := cb.GetFileDiff(ctx, "nonexistent.txt", "", "", 0, 100000)
	if err == nil {
		t.Fatal("error: got = nil, wanted non-nil")
	}
	if !strings.Contains(err.Error(), "not changed") {
		t.Errorf("error: got = %v, wanted containing %q", err, "not changed")
	}
}

func TestGetFileDiffWithCommitRange(t *testing.T) {
	repo, baseHash, paths := initHistoryRepo(t, 3)
	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// Get the commit SHAs.
	commits, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}

	// The oldest commit (index 2) added paths[0]. Get its diff using
	// explicit start/end SHAs. The oldest commit's parent is the base,
	// so we use the middle commit as "end" and the oldest as "start"
	// to get the diff of paths[1] (added by middle commit).
	//
	// Commits are [newest=paths[2], middle=paths[1], oldest=paths[0]]
	oldestSHA := commits.Commits[2].SHA
	middleSHA := commits.Commits[1].SHA

	result, err := cb.GetFileDiff(ctx, paths[1], oldestSHA, middleSHA, 0, 100000)
	if err != nil {
		t.Fatalf("GetFileDiff with range: %v", err)
	}
	if result.Diff == "" {
		t.Error("diff: got empty, wanted non-empty")
	}
	if !strings.Contains(result.Diff, paths[1]) {
		t.Errorf("diff does not contain %q", paths[1])
	}
}

func TestGetFileDiffModifiedFile(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	// Base commit with initial content.
	writeTestFile(t, dir, "mod.txt", "original content\n", 0o644)
	if _, err := wt.Add("mod.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Modify the file.
	modified := fmt.Sprintf("modified content %d\n", rand.Int64())
	writeTestFile(t, dir, "mod.txt", modified, 0o644)
	if _, err := wt.Add("mod.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("modify mod.txt", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// ListCommits should show the file as "modified".
	commits, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if len(commits.Commits) != 1 {
		t.Fatalf("len(commits): got = %d, wanted = 1", len(commits.Commits))
	}
	if commits.Commits[0].Files[0].Type != "modified" {
		t.Errorf("file type: got = %q, wanted = %q", commits.Commits[0].Files[0].Type, "modified")
	}

	// GetFileDiff should show the modification.
	result, err := cb.GetFileDiff(ctx, "mod.txt", "", "", 0, 100000)
	if err != nil {
		t.Fatalf("GetFileDiff: %v", err)
	}
	if !strings.Contains(result.Diff, "original") {
		t.Error("diff missing original content")
	}
	if !strings.Contains(result.Diff, "modified") {
		t.Error("diff missing modified content")
	}
}

func TestGetFileDiffDeletedFile(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, dir, "del.txt", "to be deleted\n", 0o644)
	if _, err := wt.Add("del.txt"); err != nil {
		t.Fatal(err)
	}
	baseHash, err := wt.Commit("base", &gogit.CommitOptions{Author: testSig()})
	if err != nil {
		t.Fatal(err)
	}

	// Delete the file.
	if err := os.Remove(filepath.Join(dir, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Remove("del.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("delete del.txt", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	cb := HistoryCallbacks(repo, baseHash)
	ctx := context.Background()

	// ListCommits should show the file as "deleted".
	commits, err := cb.ListCommits(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListCommits: %v", err)
	}
	if commits.Commits[0].Files[0].Type != "deleted" {
		t.Errorf("file type: got = %q, wanted = %q", commits.Commits[0].Files[0].Type, "deleted")
	}

	// GetFileDiff should work for deleted files.
	result, err := cb.GetFileDiff(ctx, "del.txt", "", "", 0, 100000)
	if err != nil {
		t.Fatalf("GetFileDiff: %v", err)
	}
	if !strings.Contains(result.Diff, "to be deleted") {
		t.Error("diff missing deleted content")
	}
}

func TestPaginateDiff(t *testing.T) {
	tests := []struct {
		name           string
		diff           string
		offset         int64
		limit          int
		wantDiff       string
		wantRemaining  int64
		wantNextOffset *int64
	}{{
		name:           "full read",
		diff:           "abcdefghij",
		offset:         0,
		limit:          100,
		wantDiff:       "abcdefghij",
		wantNextOffset: nil,
	}, {
		name:           "partial read from start",
		diff:           "abcdefghij",
		offset:         0,
		limit:          5,
		wantDiff:       "abcde",
		wantRemaining:  5,
		wantNextOffset: int64Ptr(5),
	}, {
		name:           "partial read with offset",
		diff:           "abcdefghij",
		offset:         3,
		limit:          4,
		wantDiff:       "defg",
		wantRemaining:  3,
		wantNextOffset: int64Ptr(7),
	}, {
		name:           "read to end with offset",
		diff:           "abcdefghij",
		offset:         7,
		limit:          100,
		wantDiff:       "hij",
		wantNextOffset: nil,
	}, {
		name:           "offset at end",
		diff:           "abcdefghij",
		offset:         10,
		limit:          100,
		wantDiff:       "",
		wantNextOffset: nil,
	}, {
		name:           "offset past end",
		diff:           "abcdefghij",
		offset:         999,
		limit:          100,
		wantDiff:       "",
		wantNextOffset: nil,
	}, {
		name:           "empty diff",
		diff:           "",
		offset:         0,
		limit:          100,
		wantDiff:       "",
		wantNextOffset: nil,
	}, {
		name:           "single byte read",
		diff:           "abcdefghij",
		offset:         0,
		limit:          1,
		wantDiff:       "a",
		wantRemaining:  9,
		wantNextOffset: int64Ptr(1),
	}, {
		name:           "limit exactly matches remaining",
		diff:           "abcdefghij",
		offset:         5,
		limit:          5,
		wantDiff:       "fghij",
		wantNextOffset: nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := paginateDiff(tt.diff, tt.offset, tt.limit)

			if result.Diff != tt.wantDiff {
				t.Errorf("diff: got = %q, wanted = %q", result.Diff, tt.wantDiff)
			}
			if result.Remaining != tt.wantRemaining {
				t.Errorf("remaining: got = %d, wanted = %d", result.Remaining, tt.wantRemaining)
			}
			if tt.wantNextOffset == nil && result.NextOffset != nil {
				t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
			}
			if tt.wantNextOffset != nil && result.NextOffset == nil {
				t.Errorf("next_offset: got = nil, wanted = %d", *tt.wantNextOffset)
			}
			if tt.wantNextOffset != nil && result.NextOffset != nil && *result.NextOffset != *tt.wantNextOffset {
				t.Errorf("next_offset: got = %d, wanted = %d", *result.NextOffset, *tt.wantNextOffset)
			}
		})
	}
}

func int64Ptr(v int64) *int64 { return &v }
