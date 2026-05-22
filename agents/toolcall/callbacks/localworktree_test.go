/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// openTestRoot creates a temporary directory, writes the supplied files into
// it, opens an os.Root on it, and returns the root and a cleanup function.
func openTestRoot(t *testing.T, files map[string]string) *os.Root {
	t.Helper()
	dir := t.TempDir()
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", relPath, err)
		}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func randStr() string { return fmt.Sprintf("val-%d", rand.Int64()) }

// ---------------------------------------------------------------------------
// ReadFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_ReadFile(t *testing.T) {
	content := randStr() + "\n" + randStr()
	r := openTestRoot(t, map[string]string{"file.txt": content})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("whole file", func(t *testing.T) {
		got, err := cb.ReadFile(ctx, "file.txt", 0, -1)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Content != content {
			t.Errorf("content: got = %q, want = %q", got.Content, content)
		}
		if got.NextOffset != nil {
			t.Errorf("next_offset: got = %v, want = nil", *got.NextOffset)
		}
		if got.Remaining != 0 {
			t.Errorf("remaining: got = %d, want = 0", got.Remaining)
		}
	})

	t.Run("partial read with next_offset", func(t *testing.T) {
		limit := 4
		got, err := cb.ReadFile(ctx, "file.txt", 0, limit)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Content != content[:limit] {
			t.Errorf("content: got = %q, want = %q", got.Content, content[:limit])
		}
		if got.NextOffset == nil {
			t.Fatal("next_offset: got = nil, want non-nil")
		}
		if *got.NextOffset != int64(limit) {
			t.Errorf("next_offset: got = %d, want = %d", *got.NextOffset, limit)
		}
		want := int64(len(content) - limit)
		if got.Remaining != want {
			t.Errorf("remaining: got = %d, want = %d", got.Remaining, want)
		}
	})

	t.Run("read from offset", func(t *testing.T) {
		offset := int64(3)
		got, err := cb.ReadFile(ctx, "file.txt", offset, -1)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Content != content[offset:] {
			t.Errorf("content: got = %q, want = %q", got.Content, content[offset:])
		}
	})

	t.Run("offset beyond EOF", func(t *testing.T) {
		got, err := cb.ReadFile(ctx, "file.txt", int64(len(content)+1), -1)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Content != "" {
			t.Errorf("content: got = %q, want = empty", got.Content)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := cb.ReadFile(ctx, "no-such-file.txt", 0, -1)
		if err == nil {
			t.Error("want error for missing file, got nil")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		_, err := cb.ReadFile(ctx, "../outside.txt", 0, -1)
		if err == nil {
			t.Error("want error for path traversal, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// WriteFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_WriteFile(t *testing.T) {
	r := openTestRoot(t, nil)
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("create new file", func(t *testing.T) {
		content := randStr()
		if err := cb.WriteFile(ctx, "new.txt", content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := r.ReadFile("new.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != content {
			t.Errorf("content: got = %q, want = %q", got, content)
		}
	})

	t.Run("overwrite existing file", func(t *testing.T) {
		_ = r.WriteFile("over.txt", []byte("old"), 0o644)
		newContent := randStr()
		if err := cb.WriteFile(ctx, "over.txt", newContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := r.ReadFile("over.txt")
		if string(got) != newContent {
			t.Errorf("content: got = %q, want = %q", got, newContent)
		}
	})

	t.Run("creates parent directories", func(t *testing.T) {
		if err := cb.WriteFile(ctx, "a/b/c/deep.txt", "hello", 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := r.ReadFile("a/b/c/deep.txt")
		if string(got) != "hello" {
			t.Errorf("content: got = %q, want = %q", got, "hello")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		if err := cb.WriteFile(ctx, "../escape.txt", "bad", 0o644); err == nil {
			t.Error("want error for path traversal, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// EditFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_EditFile(t *testing.T) {
	r := openTestRoot(t, map[string]string{
		"edit.txt": "hello world hello",
	})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("replace single occurrence", func(t *testing.T) {
		_ = r.WriteFile("edit.txt", []byte("hello world hello"), 0o644)
		res, err := cb.EditFile(ctx, "edit.txt", "world", "earth", false)
		if err != nil {
			t.Fatalf("EditFile: %v", err)
		}
		if res.Replacements != 1 {
			t.Errorf("replacements: got = %d, want = 1", res.Replacements)
		}
		got, _ := r.ReadFile("edit.txt")
		if string(got) != "hello earth hello" {
			t.Errorf("content: got = %q, want = %q", got, "hello earth hello")
		}
	})

	t.Run("replace_all replaces all occurrences", func(t *testing.T) {
		_ = r.WriteFile("edit.txt", []byte("hello world hello"), 0o644)
		res, err := cb.EditFile(ctx, "edit.txt", "hello", "hi", true)
		if err != nil {
			t.Fatalf("EditFile: %v", err)
		}
		if res.Replacements != 2 {
			t.Errorf("replacements: got = %d, want = 2", res.Replacements)
		}
		got, _ := r.ReadFile("edit.txt")
		if string(got) != "hi world hi" {
			t.Errorf("content: got = %q, want = %q", got, "hi world hi")
		}
	})

	t.Run("error when string not found", func(t *testing.T) {
		_ = r.WriteFile("edit.txt", []byte("hello world"), 0o644)
		_, err := cb.EditFile(ctx, "edit.txt", "missing", "x", false)
		if err == nil {
			t.Error("want error for missing string, got nil")
		}
	})

	t.Run("error when not unique and replace_all false", func(t *testing.T) {
		_ = r.WriteFile("edit.txt", []byte("aaa"), 0o644)
		_, err := cb.EditFile(ctx, "edit.txt", "a", "b", false)
		if err == nil {
			t.Error("want error for non-unique match, got nil")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := cb.EditFile(ctx, "no-such.txt", "x", "y", false)
		if err == nil {
			t.Error("want error for missing file, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// DeleteFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_DeleteFile(t *testing.T) {
	r := openTestRoot(t, map[string]string{"del.txt": "bye"})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("deletes existing file", func(t *testing.T) {
		if err := cb.DeleteFile(ctx, "del.txt"); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		if _, err := r.Stat("del.txt"); err == nil {
			t.Error("file still exists after delete")
		}
	})

	t.Run("error for missing file", func(t *testing.T) {
		if err := cb.DeleteFile(ctx, "no-such.txt"); err == nil {
			t.Error("want error, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// MoveFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_MoveFile(t *testing.T) {
	r := openTestRoot(t, map[string]string{"src.txt": "contents"})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("moves file", func(t *testing.T) {
		if err := cb.MoveFile(ctx, "src.txt", "dst.txt"); err != nil {
			t.Fatalf("MoveFile: %v", err)
		}
		if _, err := r.Stat("src.txt"); err == nil {
			t.Error("source still exists after move")
		}
		got, err := r.ReadFile("dst.txt")
		if err != nil {
			t.Fatalf("ReadFile dst: %v", err)
		}
		if string(got) != "contents" {
			t.Errorf("content: got = %q, want = %q", got, "contents")
		}
	})
}

// ---------------------------------------------------------------------------
// CopyFile
// ---------------------------------------------------------------------------

func TestLocalWorktree_CopyFile(t *testing.T) {
	content := randStr()
	r := openTestRoot(t, map[string]string{"orig.txt": content})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("copies file", func(t *testing.T) {
		if err := cb.CopyFile(ctx, "orig.txt", "copy.txt"); err != nil {
			t.Fatalf("CopyFile: %v", err)
		}
		// Both files should exist with same content.
		orig, _ := r.ReadFile("orig.txt")
		copy, _ := r.ReadFile("copy.txt")
		if string(orig) != content {
			t.Errorf("original: got = %q, want = %q", orig, content)
		}
		if string(copy) != content {
			t.Errorf("copy: got = %q, want = %q", copy, content)
		}
	})

	t.Run("missing source", func(t *testing.T) {
		if err := cb.CopyFile(ctx, "no-such.txt", "dst.txt"); err == nil {
			t.Error("want error for missing source, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// Chmod
// ---------------------------------------------------------------------------

func TestLocalWorktree_Chmod(t *testing.T) {
	r := openTestRoot(t, map[string]string{"script.sh": "#!/bin/sh"})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("sets executable bit", func(t *testing.T) {
		if err := cb.Chmod(ctx, "script.sh", 0o755); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		info, err := r.Stat("script.sh")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("mode: got = %o, want executable bits set", info.Mode())
		}
	})
}

// ---------------------------------------------------------------------------
// ListDirectory
// ---------------------------------------------------------------------------

func TestLocalWorktree_ListDirectory(t *testing.T) {
	r := openTestRoot(t, map[string]string{
		"a.go":     "package a",
		"b.go":     "package b",
		"c.txt":    "text",
		"sub/d.go": "package d",
	})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("lists root", func(t *testing.T) {
		res, err := cb.ListDirectory(ctx, ".", "", 0, 50)
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		names := make(map[string]struct{})
		for _, e := range res.Entries {
			names[e.Name] = struct{}{}
		}
		for _, want := range []string{"a.go", "b.go", "c.txt", "sub"} {
			if _, exists := names[want]; !exists {
				t.Errorf("missing entry %q in listing", want)
			}
		}
	})

	t.Run("filter by glob", func(t *testing.T) {
		res, err := cb.ListDirectory(ctx, ".", "*.go", 0, 50)
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		for _, e := range res.Entries {
			if e.Type == "file" && filepath.Ext(e.Name) != ".go" {
				t.Errorf("unexpected non-.go file in filtered listing: %q", e.Name)
			}
		}
		if len(res.Entries) != 2 {
			t.Errorf("entries: got = %d, want = 2", len(res.Entries))
		}
	})

	t.Run("pagination offset and limit", func(t *testing.T) {
		// Get all entries first to know total count.
		all, _ := cb.ListDirectory(ctx, ".", "", 0, 50)
		total := len(all.Entries)
		if total < 2 {
			t.Skip("need at least 2 entries to test pagination")
		}
		page1, err := cb.ListDirectory(ctx, ".", "", 0, 1)
		if err != nil {
			t.Fatalf("page1: %v", err)
		}
		if len(page1.Entries) != 1 {
			t.Errorf("page1 entries: got = %d, want = 1", len(page1.Entries))
		}
		if page1.NextOffset == nil {
			t.Fatal("page1 next_offset: got = nil, want non-nil")
		}
		if *page1.NextOffset != 1 {
			t.Errorf("page1 next_offset: got = %d, want = 1", *page1.NextOffset)
		}
		if page1.Remaining != total-1 {
			t.Errorf("page1 remaining: got = %d, want = %d", page1.Remaining, total-1)
		}
	})

	t.Run("offset beyond end returns empty", func(t *testing.T) {
		res, err := cb.ListDirectory(ctx, ".", "", 9999, 50)
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		if len(res.Entries) != 0 {
			t.Errorf("entries: got = %d, want = 0", len(res.Entries))
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		_, err := cb.ListDirectory(ctx, "no-such-dir", "", 0, 50)
		if err == nil {
			t.Error("want error for missing directory, got nil")
		}
	})
}

// ---------------------------------------------------------------------------
// SearchCodebase
// ---------------------------------------------------------------------------

func TestLocalWorktree_SearchCodebase(t *testing.T) {
	r := openTestRoot(t, map[string]string{
		"foo.go":     "package foo\n\nfunc Foo() {}\nfunc Bar() {}\n",
		"bar.go":     "package bar\n\nfunc FooBar() {}\n",
		"sub/baz.go": "package baz\n\nfunc Foo() {}\n",
		".hidden/x":  "func Foo() {}", // should be skipped (hidden dir)
	})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("finds all matches across files", func(t *testing.T) {
		res, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, 50)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		// foo.go, bar.go (FooBar), sub/baz.go — hidden dir skipped.
		if len(res.Matches) != 3 {
			t.Errorf("matches: got = %d, want = 3", len(res.Matches))
		}
	})

	t.Run("filter by glob limits files searched", func(t *testing.T) {
		res, err := cb.SearchCodebase(ctx, ".", `func Foo`, "foo.go", 0, 50)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		for _, m := range res.Matches {
			if filepath.Base(m.Path) != "foo.go" {
				t.Errorf("unexpected match in %q (want foo.go only)", m.Path)
			}
		}
	})

	t.Run("pagination", func(t *testing.T) {
		all, _ := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, 50)
		total := len(all.Matches)
		if total < 2 {
			t.Skip("need at least 2 matches to test pagination")
		}
		page, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, 1)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		if len(page.Matches) != 1 {
			t.Errorf("matches: got = %d, want = 1", len(page.Matches))
		}
		if !page.HasMore {
			t.Error("has_more: got = false, want = true")
		}
		if page.NextOffset == nil {
			t.Fatal("next_offset: got = nil, want non-nil")
		}
		if *page.NextOffset != 1 {
			t.Errorf("next_offset: got = %d, want = 1", *page.NextOffset)
		}
	})

	t.Run("offset beyond matches returns empty", func(t *testing.T) {
		res, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 9999, 50)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		if len(res.Matches) != 0 {
			t.Errorf("matches: got = %d, want = 0", len(res.Matches))
		}
	})

	t.Run("invalid pattern returns error", func(t *testing.T) {
		_, err := cb.SearchCodebase(ctx, ".", `[invalid`, "", 0, 50)
		if err == nil {
			t.Error("want error for invalid regex, got nil")
		}
	})

	t.Run("match offsets are correct", func(t *testing.T) {
		// Write a file with known content so we can verify offsets.
		_ = r.WriteFile("offsets.go", []byte("aXbXcX"), 0o644)
		res, err := cb.SearchCodebase(ctx, ".", `X`, "offsets.go", 0, 50)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		wantOffsets := []int64{1, 3, 5}
		if len(res.Matches) != len(wantOffsets) {
			t.Fatalf("matches: got = %d, want = %d", len(res.Matches), len(wantOffsets))
		}
		for i, m := range res.Matches {
			if m.Offset != wantOffsets[i] {
				t.Errorf("match[%d].offset: got = %d, want = %d", i, m.Offset, wantOffsets[i])
			}
			if m.Length != 1 {
				t.Errorf("match[%d].length: got = %d, want = 1", i, m.Length)
			}
		}
	})

	t.Run("hidden directories skipped", func(t *testing.T) {
		res, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, 50)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		for _, m := range res.Matches {
			if strings.HasPrefix(m.Path, ".") || strings.Contains(m.Path, "/.") {
				t.Errorf("match in hidden path %q", m.Path)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// CreateSymlink (basic smoke test — full symlink validation is OS-dependent)
// ---------------------------------------------------------------------------

func TestLocalWorktree_CreateSymlink(t *testing.T) {
	r := openTestRoot(t, map[string]string{"target.txt": "target content"})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("creates relative symlink", func(t *testing.T) {
		if err := cb.CreateSymlink(ctx, "link.txt", "target.txt"); err != nil {
			t.Fatalf("CreateSymlink: %v", err)
		}
		// The link should be readable through the root.
		got, err := r.ReadFile("link.txt")
		if err != nil {
			t.Fatalf("ReadFile through symlink: %v", err)
		}
		if string(got) != "target content" {
			t.Errorf("content: got = %q, want = %q", got, "target content")
		}
	})
}
