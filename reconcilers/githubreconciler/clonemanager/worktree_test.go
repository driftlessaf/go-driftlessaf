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

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initWorktree creates a temporary git repo with test fixtures and returns
// the worktree and root path.
func initWorktree(t *testing.T) (*gogit.Worktree, string) {
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

	// hello.txt — small known content for offset tests.
	writeTestFile(t, dir, "hello.txt", "Hello, World!", 0o644)

	// big.txt — larger file for pagination/windowed reads.
	writeTestFile(t, dir, "big.txt", strings.Repeat("abcdefghij", 100), 0o644)

	// subdir/nested.txt — nested file for path/search scoping.
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "subdir/nested.txt", "nested content here", 0o644)

	// script.sh — executable file for chmod/mode tests.
	writeTestFile(t, dir, "script.sh", "#!/bin/sh\necho hi\n", 0o755)

	// image.png — empty binary file for binary detection tests.
	writeTestFile(t, dir, "image.png", "", 0o644)

	// Stage and commit everything.
	if _, err := wt.Add("."); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	return wt, dir
}

func writeTestFile(t *testing.T, dir, relPath, content string, mode os.FileMode) {
	t.Helper()
	fullPath := filepath.Join(dir, relPath)
	if err := os.WriteFile(fullPath, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	tests := []struct {
		name          string
		path          string
		offset        int64
		limit         int
		wantContent   string
		wantRemaining int64
		wantNextNil   bool
		wantErr       string
	}{{
		name:        "full read",
		path:        "hello.txt",
		offset:      0,
		limit:       -1,
		wantContent: "Hello, World!",
		wantNextNil: true,
	}, {
		name:          "windowed read from start",
		path:          "hello.txt",
		offset:        0,
		limit:         5,
		wantContent:   "Hello",
		wantRemaining: 8, // ", World!" = 8 bytes
		wantNextNil:   false,
	}, {
		name:          "windowed read with offset",
		path:          "hello.txt",
		offset:        7,
		limit:         5,
		wantContent:   "World",
		wantRemaining: 1, // "!" = 1 byte
		wantNextNil:   false,
	}, {
		name:        "read to end with offset",
		path:        "hello.txt",
		offset:      7,
		limit:       -1,
		wantContent: "World!",
		wantNextNil: true,
	}, {
		name:        "offset past EOF",
		path:        "hello.txt",
		offset:      9999,
		limit:       -1,
		wantContent: "",
		wantNextNil: true,
	}, {
		name:    "binary file rejected",
		path:    "image.png",
		offset:  0,
		limit:   -1,
		wantErr: "binary",
	}, {
		name:    "nonexistent file",
		path:    "nope.txt",
		offset:  0,
		limit:   -1,
		wantErr: "no such file",
	}, {
		name:    "path escape",
		path:    "../../etc/passwd",
		offset:  0,
		limit:   -1,
		wantErr: "escapes worktree",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := cb.ReadFile(ctx, tc.path, tc.offset, tc.limit)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Content != tc.wantContent {
				t.Errorf("content: got = %q, wanted = %q", result.Content, tc.wantContent)
			}
			if result.Remaining != tc.wantRemaining {
				t.Errorf("remaining: got = %d, wanted = %d", result.Remaining, tc.wantRemaining)
			}
			if tc.wantNextNil && result.NextOffset != nil {
				t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
			}
			if !tc.wantNextNil && result.NextOffset == nil {
				t.Error("next_offset: got = nil, wanted non-nil")
			}
		})
	}
}

func TestReadFileContinuation(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	// Read big.txt in chunks and reassemble.
	var assembled strings.Builder
	var offset int64
	const chunkSize = 37 // odd size to test boundary handling

	for range 1000 { // guard against infinite loop
		result, err := cb.ReadFile(ctx, "big.txt", offset, chunkSize)
		if err != nil {
			t.Fatalf("read at offset %d: %v", offset, err)
		}
		assembled.WriteString(result.Content)
		if result.NextOffset == nil {
			break
		}
		offset = *result.NextOffset
	}

	want := strings.Repeat("abcdefghij", 100)
	if assembled.String() != want {
		t.Errorf("reassembled content length: got = %d, wanted = %d", assembled.Len(), len(want))
	}
}

func TestWriteFile(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	content := fmt.Sprintf("written-%d", rand.Int64())

	t.Run("create new file", func(t *testing.T) {
		if err := cb.WriteFile(ctx, "newfile.txt", content, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(root, "newfile.txt"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != content {
			t.Errorf("content: got = %q, wanted = %q", got, content)
		}
		assertStaged(t, wt, "newfile.txt")
	})

	t.Run("overwrite existing", func(t *testing.T) {
		newContent := fmt.Sprintf("overwritten-%d", rand.Int64())
		if err := cb.WriteFile(ctx, "hello.txt", newContent, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(root, "hello.txt"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != newContent {
			t.Errorf("content: got = %q, wanted = %q", got, newContent)
		}
	})

	t.Run("implicit mkdir", func(t *testing.T) {
		if err := cb.WriteFile(ctx, "deep/nested/file.txt", "deep", 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(root, "deep", "nested", "file.txt"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != "deep" {
			t.Errorf("content: got = %q, wanted = %q", got, "deep")
		}
		assertStaged(t, wt, "deep/nested/file.txt")
	})
}

func TestDeleteFile(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	if err := cb.DeleteFile(ctx, "hello.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "hello.txt")); !os.IsNotExist(err) {
		t.Error("file still exists after delete")
	}

	// Verify removed from git index.
	status, err := wt.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	st, ok := status["hello.txt"]
	if !ok {
		// Not in status at all is also acceptable (fully removed).
		return
	}
	if st.Staging != gogit.Deleted {
		t.Errorf("staging status: got = %v, wanted = %v", st.Staging, gogit.Deleted)
	}
}

func TestMoveFile(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	if err := cb.MoveFile(ctx, "hello.txt", "moved/hello.txt"); err != nil {
		t.Fatalf("move: %v", err)
	}

	// Old path should not exist.
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); !os.IsNotExist(err) {
		t.Error("source file still exists after move")
	}

	// New path should contain original content.
	got, err := os.ReadFile(filepath.Join(root, "moved", "hello.txt"))
	if err != nil {
		t.Fatalf("read moved file: %v", err)
	}
	if string(got) != "Hello, World!" {
		t.Errorf("content: got = %q, wanted = %q", got, "Hello, World!")
	}

	assertStaged(t, wt, "moved/hello.txt")
}

func TestCopyFile(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	if err := cb.CopyFile(ctx, "script.sh", "copy.sh"); err != nil {
		t.Fatalf("copy: %v", err)
	}

	// Source still exists.
	if _, err := os.Stat(filepath.Join(root, "script.sh")); err != nil {
		t.Error("source file missing after copy")
	}

	// Destination has same content.
	got, err := os.ReadFile(filepath.Join(root, "copy.sh"))
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(got) != "#!/bin/sh\necho hi\n" {
		t.Errorf("content: got = %q, wanted = %q", got, "#!/bin/sh\necho hi\n")
	}

	// Destination preserves mode.
	fi, err := os.Stat(filepath.Join(root, "copy.sh"))
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("copy lost executable bit")
	}

	assertStaged(t, wt, "copy.sh")
}

func TestCreateSymlink(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	if err := cb.CreateSymlink(ctx, "link.txt", "hello.txt"); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	target, err := os.Readlink(filepath.Join(root, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "hello.txt" {
		t.Errorf("symlink target: got = %q, wanted = %q", target, "hello.txt")
	}

	assertStaged(t, wt, "link.txt")
}

func TestCreateSymlinkEscape(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	tests := []struct {
		name    string
		path    string
		target  string
		wantErr string
	}{{
		name:    "absolute target",
		path:    "abs.txt",
		target:  "/etc/passwd",
		wantErr: "absolute",
	}, {
		name:    "relative escape",
		path:    "escape.txt",
		target:  "../../../etc/passwd",
		wantErr: "outside worktree",
	}, {
		name:    "nested relative escape",
		path:    "subdir/escape.txt",
		target:  "../../../../../../etc/passwd",
		wantErr: "outside worktree",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cb.CreateSymlink(ctx, tc.path, tc.target)
			if err == nil {
				t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestChmod(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	if err := cb.Chmod(ctx, "hello.txt", 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("executable bit not set after chmod 0755")
	}

	assertStaged(t, wt, "hello.txt")
}

func TestListDirectory(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	tests := []struct {
		name          string
		path          string
		filter        string
		offset        int
		limit         int
		wantNames     []string
		wantRemaining int
		wantNextNil   bool
	}{{
		name:        "root listing",
		path:        ".",
		filter:      "",
		offset:      0,
		limit:       100,
		wantNextNil: true,
	}, {
		name:        "glob filter txt",
		path:        ".",
		filter:      "*.txt",
		offset:      0,
		limit:       100,
		wantNames:   []string{"big.txt", "hello.txt"},
		wantNextNil: true,
	}, {
		name:        "exact filter",
		path:        ".",
		filter:      "script.sh",
		offset:      0,
		limit:       100,
		wantNames:   []string{"script.sh"},
		wantNextNil: true,
	}, {
		name:          "pagination first page",
		path:          ".",
		filter:        "",
		offset:        0,
		limit:         2,
		wantRemaining: -1, // checked separately
		wantNextNil:   false,
	}, {
		name:        "offset past end",
		path:        ".",
		filter:      "",
		offset:      9999,
		limit:       10,
		wantNames:   nil,
		wantNextNil: true,
	}, {
		name:        "subdir listing",
		path:        "subdir",
		filter:      "",
		offset:      0,
		limit:       100,
		wantNames:   []string{"nested.txt"},
		wantNextNil: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := cb.ListDirectory(ctx, tc.path, tc.filter, tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("list: %v", err)
			}

			if tc.wantNames != nil {
				gotNames := make([]string, 0, len(result.Entries))
				for _, e := range result.Entries {
					gotNames = append(gotNames, e.Name)
				}
				if len(gotNames) != len(tc.wantNames) {
					t.Fatalf("entries: got = %v, wanted = %v", gotNames, tc.wantNames)
				}
				for i := range tc.wantNames {
					if gotNames[i] != tc.wantNames[i] {
						t.Errorf("entry[%d]: got = %q, wanted = %q", i, gotNames[i], tc.wantNames[i])
					}
				}
			}

			if tc.wantNextNil && result.NextOffset != nil {
				t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
			}
			if !tc.wantNextNil && result.NextOffset == nil {
				t.Error("next_offset: got = nil, wanted non-nil")
			}
			if tc.wantRemaining >= 0 && result.Remaining != tc.wantRemaining {
				t.Errorf("remaining: got = %d, wanted = %d", result.Remaining, tc.wantRemaining)
			}
		})
	}
}

func TestListDirectoryMetadata(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	result, err := cb.ListDirectory(ctx, ".", "", 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	entryMap := make(map[string]callbacks.DirEntry, len(result.Entries))
	for _, e := range result.Entries {
		entryMap[e.Name] = e
	}

	// Check file type and size.
	if e, ok := entryMap["hello.txt"]; ok {
		if e.Type != "file" {
			t.Errorf("hello.txt type: got = %q, wanted = %q", e.Type, "file")
		}
		if e.Size != 13 { // len("Hello, World!")
			t.Errorf("hello.txt size: got = %d, wanted = %d", e.Size, 13)
		}
	} else {
		t.Error("hello.txt not found in listing")
	}

	// Check directory type.
	if e, ok := entryMap["subdir"]; ok {
		if e.Type != "directory" {
			t.Errorf("subdir type: got = %q, wanted = %q", e.Type, "directory")
		}
		if e.Size != 0 {
			t.Errorf("subdir size: got = %d, wanted = %d", e.Size, 0)
		}
	} else {
		t.Error("subdir not found in listing")
	}
}

func TestSearchCodebase(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	tests := []struct {
		name          string
		path          string
		pattern       string
		filter        string
		offset        int
		limit         int
		wantMinCount  int
		wantRemaining int
		wantNextNil   bool
		wantErr       string
	}{{
		name:         "basic match",
		path:         ".",
		pattern:      "Hello",
		filter:       "",
		offset:       0,
		limit:        100,
		wantMinCount: 1,
		wantNextNil:  true,
	}, {
		name:         "regex match",
		path:         ".",
		pattern:      "Hello.*World",
		filter:       "",
		offset:       0,
		limit:        100,
		wantMinCount: 1,
		wantNextNil:  true,
	}, {
		name:         "file filter",
		path:         ".",
		pattern:      "content",
		filter:       "*.txt",
		offset:       0,
		limit:        100,
		wantMinCount: 1,
		wantNextNil:  true,
	}, {
		name:         "path scoping",
		path:         "subdir",
		pattern:      "nested",
		filter:       "",
		offset:       0,
		limit:        100,
		wantMinCount: 1,
		wantNextNil:  true,
	}, {
		name:        "no matches",
		path:        ".",
		pattern:     "zzz_no_match_zzz",
		filter:      "",
		offset:      0,
		limit:       100,
		wantNextNil: true,
	}, {
		name:        "binary files skipped",
		path:        ".",
		pattern:     ".*",
		filter:      "*.png",
		offset:      0,
		limit:       100,
		wantNextNil: true,
	}, {
		name:    "invalid regex",
		path:    ".",
		pattern: "[invalid",
		filter:  "",
		offset:  0,
		limit:   100,
		wantErr: "invalid pattern",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := cb.SearchCodebase(ctx, tc.path, tc.pattern, tc.filter, tc.offset, tc.limit)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("search: %v", err)
			}

			if len(result.Matches) < tc.wantMinCount {
				t.Errorf("match count: got = %d, wanted >= %d", len(result.Matches), tc.wantMinCount)
			}

			if tc.wantNextNil && result.NextOffset != nil {
				t.Errorf("next_offset: got = %d, wanted = nil", *result.NextOffset)
			}
			if !tc.wantNextNil && result.NextOffset == nil {
				t.Error("next_offset: got = nil, wanted non-nil")
			}
		})
	}
}

func TestSearchCodebasePagination(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	// big.txt has "abcdefghij" repeated 100 times. Search for "abc" which
	// should match 100 times.
	full, err := cb.SearchCodebase(ctx, ".", "abc", "big.txt", 0, 1000)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(full.Matches) != 100 {
		t.Fatalf("total matches: got = %d, wanted = %d", len(full.Matches), 100)
	}

	// First page.
	page1, err := cb.SearchCodebase(ctx, ".", "abc", "big.txt", 0, 10)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Matches) != 10 {
		t.Errorf("page1 count: got = %d, wanted = %d", len(page1.Matches), 10)
	}
	if !page1.HasMore {
		t.Error("page1 has_more: got = false, wanted = true")
	}
	if page1.NextOffset == nil {
		t.Fatal("page1 next_offset: got = nil, wanted non-nil")
	}

	// Second page using NextOffset.
	page2, err := cb.SearchCodebase(ctx, ".", "abc", "big.txt", *page1.NextOffset, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Matches) != 10 {
		t.Errorf("page2 count: got = %d, wanted = %d", len(page2.Matches), 10)
	}

	// First match on page2 should differ from first match on page1.
	if page1.Matches[0].Offset == page2.Matches[0].Offset {
		t.Error("page2 returned same first match as page1")
	}
}

func TestSearchCodebaseMatchOffsets(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	result, err := cb.SearchCodebase(ctx, ".", "World", "hello.txt", 0, 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("match count: got = %d, wanted = %d", len(result.Matches), 1)
	}

	m := result.Matches[0]
	if m.Path != "hello.txt" {
		t.Errorf("path: got = %q, wanted = %q", m.Path, "hello.txt")
	}
	// "Hello, World!" — "World" starts at byte 7.
	if m.Offset != 7 {
		t.Errorf("offset: got = %d, wanted = %d", m.Offset, 7)
	}
	if m.Length != 5 {
		t.Errorf("length: got = %d, wanted = %d", m.Length, 5)
	}
}

func TestValidatePath(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{{
		name: "simple file",
		path: "hello.txt",
	}, {
		name: "nested path",
		path: "a/b/c.txt",
	}, {
		name: "dot path",
		path: ".",
	}, {
		name:    "parent escape",
		path:    "../secret",
		wantErr: "escapes worktree",
	}, {
		name:    "nested escape",
		path:    "a/../../secret",
		wantErr: "escapes worktree",
	}, {
		name:    "double dot middle",
		path:    "a/b/../../../secret",
		wantErr: "escapes worktree",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fullPath, err := validatePath(root, tc.path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasPrefix(fullPath, root) {
				t.Errorf("path: got = %q, wanted prefix %q", fullPath, root)
			}
		})
	}
}

func TestValidateSymlinkTarget(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name         string
		linkFullPath string
		target       string
		wantErr      string
	}{{
		name:         "valid relative same dir",
		linkFullPath: filepath.Join(root, "link.txt"),
		target:       "hello.txt",
	}, {
		name:         "valid relative subdir",
		linkFullPath: filepath.Join(root, "subdir", "link.txt"),
		target:       "../hello.txt",
	}, {
		name:         "absolute rejected",
		linkFullPath: filepath.Join(root, "link.txt"),
		target:       "/etc/passwd",
		wantErr:      "absolute",
	}, {
		name:         "relative escape from root",
		linkFullPath: filepath.Join(root, "link.txt"),
		target:       "../../secret",
		wantErr:      "outside worktree",
	}, {
		name:         "relative escape from subdir",
		linkFullPath: filepath.Join(root, "a", "link.txt"),
		target:       "../../secret",
		wantErr:      "outside worktree",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSymlinkTarget(root, tc.linkFullPath, tc.target)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMatchFilter(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		filter string
		want   bool
	}{{
		name:   "empty filter matches everything",
		input:  "anything.txt",
		filter: "",
		want:   true,
	}, {
		name:   "glob star txt",
		input:  "hello.txt",
		filter: "*.txt",
		want:   true,
	}, {
		name:   "glob star txt no match",
		input:  "hello.go",
		filter: "*.txt",
		want:   false,
	}, {
		name:   "exact match",
		input:  "hello.txt",
		filter: "hello.txt",
		want:   true,
	}, {
		name:   "exact no match",
		input:  "hello.txt",
		filter: "world.txt",
		want:   false,
	}, {
		name:   "glob question mark not supported as glob",
		input:  "hello.txt",
		filter: "hell?.txt",
		want:   false, // No * in filter, so treated as exact match.
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchFilter(tc.input, tc.filter)
			if got != tc.want {
				t.Errorf("matchFilter(%q, %q): got = %v, wanted = %v", tc.input, tc.filter, got, tc.want)
			}
		})
	}
}

func TestAdjustUTF8Boundary(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want []byte
	}{{
		name: "valid ascii unchanged",
		buf:  []byte("hello"),
		want: []byte("hello"),
	}, {
		name: "valid multibyte unchanged",
		buf:  []byte("héllo"),
		want: []byte("héllo"),
	}, {
		name: "incomplete 2-byte trimmed",
		// é is 0xc3 0xa9 in UTF-8. Truncate after first byte.
		buf:  []byte{'h', 'e', 0xc3},
		want: []byte{'h', 'e'},
	}, {
		name: "incomplete 3-byte trimmed",
		// € is 0xe2 0x82 0xac. Truncate after second byte.
		buf:  []byte{'a', 0xe2, 0x82},
		want: []byte{'a'},
	}, {
		name: "incomplete 4-byte trimmed",
		// 𝄞 (U+1D11E) is 0xf0 0x9d 0x84 0x9e. Truncate after third byte.
		buf:  []byte{'a', 0xf0, 0x9d, 0x84},
		want: []byte{'a'},
	}, {
		name: "empty buffer",
		buf:  []byte{},
		want: []byte{},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := adjustUTF8Boundary(tc.buf)
			if string(got) != string(tc.want) {
				t.Errorf("adjustUTF8Boundary: got = %v, wanted = %v", got, tc.want)
			}
		})
	}
}

func TestEditFile(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	tests := []struct {
		name             string
		path             string
		oldString        string
		newString        string
		replaceAll       bool
		wantReplacements int
		wantContent      string
		wantErr          string
	}{{
		name:             "single replacement",
		path:             "hello.txt",
		oldString:        "World",
		newString:        fmt.Sprintf("Go-%d", rand.Int64()),
		wantReplacements: 1,
	}, {
		name:             "replace with empty deletes matched text",
		path:             "hello.txt",
		oldString:        ", ",
		newString:        "",
		wantReplacements: 1,
		wantContent:      "HelloWorld!",
	}, {
		name:             "replace with longer string",
		path:             "hello.txt",
		oldString:        "Hello",
		newString:        "Greetings and Salutations",
		wantReplacements: 1,
		wantContent:      "Greetings and Salutations, World!",
	}, {
		name:             "replace with shorter string",
		path:             "hello.txt",
		oldString:        "Hello, World!",
		newString:        "Hi",
		wantReplacements: 1,
		wantContent:      "Hi",
	}, {
		name:             "replaceAll multiple occurrences",
		path:             "big.txt",
		oldString:        "abc",
		newString:        "XYZ",
		replaceAll:       true,
		wantReplacements: 100,
	}, {
		name:      "not found",
		path:      "hello.txt",
		oldString: "zzz_no_match_zzz",
		newString: "replacement",
		wantErr:   "not found",
	}, {
		name:      "multiple without replaceAll",
		path:      "big.txt",
		oldString: "abc",
		newString: "XYZ",
		wantErr:   "more than once",
	}, {
		name:      "empty old_string",
		path:      "hello.txt",
		oldString: "",
		newString: "replacement",
		wantErr:   "must not be empty",
	}, {
		name:      "old_string too large",
		path:      "hello.txt",
		oldString: strings.Repeat("x", maxEditStringSize+1),
		newString: "replacement",
		wantErr:   "use write_file",
	}, {
		name:      "new_string too large",
		path:      "hello.txt",
		oldString: "Hello",
		newString: strings.Repeat("x", maxEditStringSize+1),
		wantErr:   "use write_file",
	}, {
		name:      "path escape",
		path:      "../../etc/passwd",
		oldString: "root",
		newString: "hacked",
		wantErr:   "escapes worktree",
	}, {
		name:      "nonexistent file",
		path:      "nope.txt",
		oldString: "hello",
		newString: "world",
		wantErr:   "no such file",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Re-init worktree for each sub-test so edits don't accumulate.
			wt, root = initWorktree(t)
			cb = WorktreeCallbacks(wt)

			result, err := cb.EditFile(ctx, tc.path, tc.oldString, tc.newString, tc.replaceAll)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error: got = nil, wanted containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got = %v, wanted containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("edit: %v", err)
			}

			if result.Replacements != tc.wantReplacements {
				t.Errorf("replacements: got = %d, wanted = %d", result.Replacements, tc.wantReplacements)
			}

			if tc.wantContent != "" {
				got, err := os.ReadFile(filepath.Join(root, tc.path))
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				if string(got) != tc.wantContent {
					t.Errorf("content: got = %q, wanted = %q", got, tc.wantContent)
				}
			}

			assertStaged(t, wt, tc.path)
		})
	}
}

func TestEditFilePreservesMode(t *testing.T) {
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	// script.sh is 0755 — verify edit preserves the executable bit.
	_, err := cb.EditFile(ctx, "script.sh", "echo hi", "echo bye", false)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}

	fi, err := os.Stat(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("executable bit lost after edit")
	}

	got, err := os.ReadFile(filepath.Join(root, "script.sh"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "#!/bin/sh\necho bye\n" {
		t.Errorf("content: got = %q, wanted = %q", got, "#!/bin/sh\necho bye\n")
	}
}

func TestEditFileDuplicateErrorContainsOffsets(t *testing.T) {
	wt, _ := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	// big.txt = "abcdefghij" * 100. "abc" appears at offsets 0, 10, 20, ...
	_, err := cb.EditFile(ctx, "big.txt", "abc", "XYZ", false)
	if err == nil {
		t.Fatal("error: got = nil, wanted non-nil")
	}

	// The error should mention the first two byte offsets.
	msg := err.Error()
	if !strings.Contains(msg, "byte offsets 0 and 10") {
		t.Errorf("error message: got = %q, wanted containing %q", msg, "byte offsets 0 and 10")
	}
}

func TestEditFileBoundarySpanning(t *testing.T) {
	// Create a file where the pattern straddles the 1MB chunk boundary.
	//
	//   ┌─────────────────────────┬─────────────────────────┐
	//   │      1MB - 3 bytes      │  remaining bytes        │
	//   │   ...padding...ABCD     │  EFGH...padding...      │
	//   └─────────────────────────┴─────────────────────────┘
	//                         ▲
	//                  chunk boundary (1<<20)
	//
	// The pattern "ABCDEFGH" starts 5 bytes before the boundary,
	// so "ABCDE" is in chunk 1 and "FGH" is in chunk 2. The
	// sliding window's overlap (patLen-1 = 7 bytes) preserves
	// "BCDE" + chunk 2 data, allowing bytes.Index to find the match.
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	pattern := "ABCDEFGH"
	replacement := fmt.Sprintf("REPLACED-%d", rand.Int64())
	boundary := 1 << 20 // 1MB

	// Place pattern so it starts 5 bytes before the boundary.
	patStart := boundary - 5
	fileSize := boundary + 1024
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = '.'
	}
	copy(data[patStart:], pattern)

	testFile := "boundary.txt"
	if err := os.WriteFile(filepath.Join(root, testFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(testFile); err != nil {
		t.Fatal(err)
	}

	result, err := cb.EditFile(ctx, testFile, pattern, replacement, false)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if result.Replacements != 1 {
		t.Errorf("replacements: got = %d, wanted = %d", result.Replacements, 1)
	}

	got, err := os.ReadFile(filepath.Join(root, testFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Verify the pattern was replaced.
	if strings.Contains(string(got), pattern) {
		t.Error("original pattern still present after edit")
	}
	if !strings.Contains(string(got), replacement) {
		t.Error("replacement not found in edited file")
	}

	// Verify file size changed by the expected delta.
	wantSize := fileSize - len(pattern) + len(replacement)
	if len(got) != wantSize {
		t.Errorf("file size: got = %d, wanted = %d", len(got), wantSize)
	}
}

func TestEditFileReplaceAllBoundary(t *testing.T) {
	// Place two occurrences of the pattern: one entirely within chunk 1,
	// one straddling the chunk boundary.
	//
	//   ┌─────────────────────────┬─────────────────────────┐
	//   │  ...PAT...padding..PAT  │ rest...padding...       │
	//   │  ↑ match 1      ↑ match 2 spans boundary         │
	//   └─────────────────────────┴─────────────────────────┘
	wt, root := initWorktree(t)
	cb := WorktreeCallbacks(wt)
	ctx := context.Background()

	pattern := "XYZXYZ"
	replacement := "NEW"
	boundary := 1 << 20

	fileSize := boundary + 1024
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = '.'
	}

	// Match 1: early in the file.
	copy(data[100:], pattern)
	// Match 2: straddling the boundary (starts 3 bytes before).
	copy(data[boundary-3:], pattern)

	testFile := "boundary_multi.txt"
	if err := os.WriteFile(filepath.Join(root, testFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(testFile); err != nil {
		t.Fatal(err)
	}

	result, err := cb.EditFile(ctx, testFile, pattern, replacement, true)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if result.Replacements != 2 {
		t.Errorf("replacements: got = %d, wanted = %d", result.Replacements, 2)
	}

	got, err := os.ReadFile(filepath.Join(root, testFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(got), pattern) {
		t.Error("original pattern still present after replaceAll edit")
	}
	if strings.Count(string(got), replacement) != 2 {
		t.Errorf("replacement count: got = %d, wanted = %d", strings.Count(string(got), replacement), 2)
	}
}

// assertStaged verifies that the given path appears as staged in the worktree.
func assertStaged(t *testing.T, wt *gogit.Worktree, path string) {
	t.Helper()
	status, err := wt.Status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	st, ok := status[path]
	if !ok {
		t.Errorf("path %q not found in git status", path)
		return
	}
	if st.Staging == gogit.Untracked || st.Staging == gogit.Unmodified {
		t.Errorf("path %q staging: got = %v, wanted staged", path, st.Staging)
	}
}
