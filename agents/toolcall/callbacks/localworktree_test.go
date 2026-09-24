/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks_test

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
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

	t.Run("partial read extends to the end of the line", func(t *testing.T) {
		// A limit that would cut the first line is extended through its
		// newline so the window holds whole lines.
		got, err := cb.ReadFile(ctx, "file.txt", 0, 4)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		lineEnd := int64(strings.IndexByte(content, '\n')) + 1
		if got.Content != content[:lineEnd] {
			t.Errorf("content: got = %q, want = %q", got.Content, content[:lineEnd])
		}
		if got.Offset != 0 {
			t.Errorf("offset: got = %d, want = 0", got.Offset)
		}
		if got.NextOffset == nil {
			t.Fatal("next_offset: got = nil, want non-nil")
		}
		if *got.NextOffset != lineEnd {
			t.Errorf("next_offset: got = %d, want = %d", *got.NextOffset, lineEnd)
		}
		want := int64(len(content)) - lineEnd
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

// linesFixture holds three whole lines. Byte offsets:
//
//	 0: "line one\n"    (9 bytes)
//	 9: "line two\n"    (9 bytes)
//	18: "line three\n"  (11 bytes)
const linesFixture = "line one\nline two\nline three\n"

func TestLocalWorktree_ReadFileLineAlignment(t *testing.T) {
	r := openTestRoot(t, map[string]string{"lines.txt": linesFixture})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	tests := []struct {
		name          string
		offset        int64
		limit         int
		wantContent   string
		wantOffset    int64
		wantNext      int64 // -1 for nil
		wantRemaining int64
	}{{
		name:          "offset mid-line moves back to the line start",
		offset:        12,
		limit:         4,
		wantContent:   "line two\n",
		wantOffset:    9,
		wantNext:      18,
		wantRemaining: 11,
	}, {
		// No newline precedes the first line, so its start is left where
		// the caller put it; a file without newlines reads byte for byte.
		name:          "offset on the first line is not moved back",
		offset:        3,
		limit:         5,
		wantContent:   "e one\n",
		wantOffset:    3,
		wantNext:      9,
		wantRemaining: 20,
	}, {
		name:          "offset at a line start stays put",
		offset:        9,
		limit:         4,
		wantContent:   "line two\n",
		wantOffset:    9,
		wantNext:      18,
		wantRemaining: 11,
	}, {
		name:          "window end extends through the newline",
		offset:        0,
		limit:         5,
		wantContent:   "line one\n",
		wantOffset:    0,
		wantNext:      9,
		wantRemaining: 20,
	}, {
		name:        "window reaching EOF is not extended",
		offset:      12,
		limit:       100,
		wantContent: "line two\nline three\n",
		wantOffset:  9,
		wantNext:    -1,
	}, {
		name:        "offset within the last line",
		offset:      20,
		limit:       -1,
		wantContent: "line three\n",
		wantOffset:  18,
		wantNext:    -1,
	}, {
		name:        "whole file",
		offset:      0,
		limit:       -1,
		wantContent: linesFixture,
		wantOffset:  0,
		wantNext:    -1,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cb.ReadFile(ctx, "lines.txt", tc.offset, tc.limit)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if got.Content != tc.wantContent {
				t.Errorf("content: got = %q, want = %q", got.Content, tc.wantContent)
			}
			if got.Offset != tc.wantOffset {
				t.Errorf("offset: got = %d, want = %d", got.Offset, tc.wantOffset)
			}
			if got.Remaining != tc.wantRemaining {
				t.Errorf("remaining: got = %d, want = %d", got.Remaining, tc.wantRemaining)
			}
			switch {
			case tc.wantNext < 0 && got.NextOffset != nil:
				t.Errorf("next_offset: got = %d, want = nil", *got.NextOffset)
			case tc.wantNext >= 0 && got.NextOffset == nil:
				t.Errorf("next_offset: got = nil, want = %d", tc.wantNext)
			case tc.wantNext >= 0 && *got.NextOffset != tc.wantNext:
				t.Errorf("next_offset: got = %d, want = %d", *got.NextOffset, tc.wantNext)
			}
		})
	}
}

func TestLocalWorktree_ReadFileChunkedWalk(t *testing.T) {
	r := openTestRoot(t, map[string]string{"lines.txt": linesFixture})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	// A small limit forces one line per window; the walk must reassemble
	// the file exactly with next_offset and remaining agreeing on the size.
	var assembled strings.Builder
	var offset int64
	for range 100 {
		got, err := cb.ReadFile(ctx, "lines.txt", offset, 4)
		if err != nil {
			t.Fatalf("ReadFile at %d: %v", offset, err)
		}
		if got.Offset != offset {
			t.Errorf("offset: got = %d, want = %d", got.Offset, offset)
		}
		assembled.WriteString(got.Content)
		if got.NextOffset == nil {
			if got.Remaining != 0 {
				t.Errorf("remaining at EOF: got = %d, want = 0", got.Remaining)
			}
			break
		}
		if sum := *got.NextOffset + got.Remaining; sum != int64(len(linesFixture)) {
			t.Errorf("next_offset + remaining: got = %d, want = %d", sum, len(linesFixture))
		}
		offset = *got.NextOffset
	}
	if assembled.String() != linesFixture {
		t.Errorf("reassembled: got = %q, want = %q", assembled.String(), linesFixture)
	}
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

// indentFixture is a tab-indented block whose inner lines an agent may copy
// with the wrong indentation. Byte offsets:
//
//	 0: "func f() {\n"
//	11: "\tif a {\n"
//	19: "\t\tb()\n"
//	25: "\n"
//	26: "\t\tc()\n"
//	32: "\t}\n"
//	35: "}\n"
const indentFixture = "func f() {\n\tif a {\n\t\tb()\n\n\t\tc()\n\t}\n}\n"

func TestLocalWorktree_EditFileIndentationShift(t *testing.T) {
	r := openTestRoot(t, nil)
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	tests := []struct {
		name        string
		content     string
		oldString   string
		newString   string
		wantContent string
		wantNote    string
		wantErr     []string
	}{{
		name:        "exact match wins over a shifted region",
		content:     "\tx()\n\ty()\n---\n\t\tx()\n\t\ty()\n",
		oldString:   "\tx()\n\ty()",
		newString:   "\tz()",
		wantContent: "\tz()\n---\n\t\tx()\n\t\ty()\n",
	}, {
		name:        "file has one more tab than old_string",
		content:     indentFixture,
		oldString:   "if a {\n\tb()\n\n\tc()\n}",
		newString:   "if a {\n\tb2()\n\n\tc2()\n}",
		wantContent: "func f() {\n\tif a {\n\t\tb2()\n\n\t\tc2()\n\t}\n}\n",
		wantNote:    "old_string matched after adjusting indentation (added 1 tab); new_string was shifted the same way",
	}, {
		name:        "file has one fewer tab than old_string",
		content:     indentFixture,
		oldString:   "\t\tif a {\n\t\t\tb()\n\n\t\t\tc()\n\t\t}",
		newString:   "\t\tif a {\n\t\t\tb2()\n\t\t}",
		wantContent: "func f() {\n\tif a {\n\t\tb2()\n\t}\n}\n",
		wantNote:    "old_string matched after adjusting indentation (removed 1 tab); new_string was shifted the same way",
	}, {
		name:        "only line endings differ yields no note",
		content:     "a\r\n\tb\r\nc\r\n",
		oldString:   "\tb\n",
		newString:   "\tB\n",
		wantContent: "a\r\n\tB\nc\r\n",
	}, {
		name:      "ambiguous shifted regions",
		content:   "\tx()\n\ty()\n---\n\t\tx()\n\t\ty()\n",
		oldString: "x()\ny()",
		newString: "z()",
		wantErr:   []string{"old_string not found in file", "at least 2 regions", "byte offsets 0 and 14"},
	}, {
		name:      "inconsistent shift",
		content:   "\tx()\n\t\ty()\n",
		oldString: "x()\ny()",
		newString: "z()",
		wantErr:   []string{"old_string not found in file", "not the same on every line"},
	}, {
		name:      "no match names the first line's location",
		content:   indentFixture,
		oldString: "if a {\n\tzzz()\n}",
		newString: "z()",
		wantErr:   []string{"old_string not found in file", "line 2 (byte offset 11)"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.WriteFile("indent.txt", []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			res, err := cb.EditFile(ctx, "indent.txt", tc.oldString, tc.newString, false)
			got, readErr := r.ReadFile("indent.txt")
			if readErr != nil {
				t.Fatalf("ReadFile: %v", readErr)
			}
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatal("EditFile: got error = nil, want non-nil")
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error: got = %q, want it to contain %q", err, w)
					}
				}
				if string(got) != tc.content {
					t.Errorf("content after failed edit: got = %q, want unchanged %q", got, tc.content)
				}
				return
			}
			if err != nil {
				t.Fatalf("EditFile: %v", err)
			}
			if res.Replacements != 1 {
				t.Errorf("replacements: got = %d, want = 1", res.Replacements)
			}
			if res.Note != tc.wantNote {
				t.Errorf("note: got = %q, want = %q", res.Note, tc.wantNote)
			}
			if string(got) != tc.wantContent {
				t.Errorf("content: got = %q, want = %q", got, tc.wantContent)
			}
		})
	}
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

	t.Run("negative limit returns every match", func(t *testing.T) {
		all, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, -1)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		if len(all.Matches) == 0 {
			t.Error("matches: got = 0, want > 0")
		}
		if all.HasMore {
			t.Error("has_more: got = true, want = false")
		}
	})
}

func TestLocalWorktree_SearchCodebaseRoot(t *testing.T) {
	r := openTestRoot(t, map[string]string{
		"top.go":      "func Foo() {}\n",
		"sub/baz.go":  "func Foo() {}\n",
		"sub/in/q.go": "func Foo() {}\n",
	})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	search := func(t *testing.T, dir string) ([]callbacks.Match, error) {
		t.Helper()
		res, err := cb.SearchCodebase(ctx, dir, `func Foo`, "", 0, -1)
		return res.Matches, err
	}

	t.Run("unusable root returns an error", func(t *testing.T) {
		for _, tc := range []struct {
			name, dir string
			wantErr   error
		}{
			{name: "nonexistent", dir: "no-such-dir", wantErr: fs.ErrNotExist},
			{name: "escapes the root", dir: "../x", wantErr: fs.ErrInvalid},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := search(t, tc.dir)
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("SearchCodebase(%q): got = %v, want error matching %v", tc.dir, err, tc.wantErr)
				}
			})
		}
	})

	t.Run("equivalent spellings match the clean path", func(t *testing.T) {
		for _, tc := range []struct{ dir, clean string }{
			{dir: "./sub", clean: "sub"},
			{dir: "sub/", clean: "sub"},
			{dir: "sub/.", clean: "sub"},
			{dir: "a/../sub", clean: "sub"},
			{dir: "", clean: "."},
		} {
			t.Run(fmt.Sprintf("%q", tc.dir), func(t *testing.T) {
				want, err := search(t, tc.clean)
				if err != nil {
					t.Fatalf("SearchCodebase(%q): %v", tc.clean, err)
				}
				if len(want) == 0 {
					t.Fatalf("SearchCodebase(%q): got = 0 matches, want > 0", tc.clean)
				}
				got, err := search(t, tc.dir)
				if err != nil {
					t.Fatalf("SearchCodebase(%q): %v", tc.dir, err)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("SearchCodebase(%q) matches (-want, +got):\n%s", tc.dir, diff)
				}
			})
		}
	})
}

func TestLocalWorktree_SearchCodebaseUnreadable(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("permission bits do not deny reads as root or on Windows")
	}
	r := openTestRoot(t, map[string]string{
		"ok.go":          "func Foo() {}\n",
		"locked.go":      "func Foo() {}\n",
		"lockeddir/x.go": "func Foo() {}\n",
	})
	for _, name := range []string{"locked.go", "lockeddir"} {
		if err := r.Chmod(name, 0o000); err != nil {
			t.Fatalf("Chmod(%q): %v", name, err)
		}
	}
	t.Cleanup(func() {
		_ = r.Chmod("locked.go", 0o644)
		_ = r.Chmod("lockeddir", 0o755)
	})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	t.Run("unreadable entries below the root are skipped", func(t *testing.T) {
		res, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", 0, -1)
		if err != nil {
			t.Fatalf("SearchCodebase: %v", err)
		}
		want := []callbacks.Match{{Path: "ok.go", Offset: 0, Length: len("func Foo")}}
		if diff := cmp.Diff(want, res.Matches); diff != "" {
			t.Errorf("matches (-want, +got):\n%s", diff)
		}
	})

	t.Run("unreadable root returns an error", func(t *testing.T) {
		if _, err := cb.SearchCodebase(ctx, "lockeddir", `func Foo`, "", 0, -1); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("SearchCodebase(lockeddir): got = %v, want error matching %v", err, fs.ErrPermission)
		}
	})
}

// ---------------------------------------------------------------------------
// Pagination offset validation
// ---------------------------------------------------------------------------

// TestLocalWorktree_NegativeOffset is a regression test: offsets arrive from
// model-generated tool calls and index slices directly, so a negative offset
// must return an error instead of panicking and killing the process.
func TestLocalWorktree_NegativeOffset(t *testing.T) {
	r := openTestRoot(t, map[string]string{"foo.go": "package foo\n\nfunc Foo() {}\n"})
	cb := callbacks.LocalWorktree(r)
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"ReadFile", func() error { _, err := cb.ReadFile(ctx, "foo.go", -1, 20000); return err }},
		{"ListDirectory", func() error { _, err := cb.ListDirectory(ctx, ".", "", -1, 50); return err }},
		{"SearchCodebase", func() error { _, err := cb.SearchCodebase(ctx, ".", `func Foo`, "", -1, 50); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("want error for negative offset, got nil")
			}
			if !strings.Contains(err.Error(), "offset must be >= 0") {
				t.Errorf("error: got = %q, want it to mention the offset bound", err)
			}
		})
	}
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
