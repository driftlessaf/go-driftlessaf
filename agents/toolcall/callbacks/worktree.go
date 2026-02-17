/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"context"
	"os"
)

// ReadResult is the result of a ReadFile callback call.
type ReadResult struct {
	// Content is the file content read from the given offset.
	Content string

	// NextOffset is the byte offset to resume reading from.
	// Nil when EOF was reached.
	NextOffset *int64

	// Remaining is the number of bytes remaining after NextOffset.
	// 0 when EOF was reached.
	Remaining int64
}

// DirEntry represents a single entry in a directory listing.
type DirEntry struct {
	// Name is the entry name (no path prefix).
	Name string

	// Size is the file size in bytes (0 for directories and symlinks).
	Size int64

	// Mode is the file mode (e.g., 0644, 0755).
	Mode os.FileMode

	// Type is "file", "directory", or "symlink".
	Type string

	// Target is the symlink target, empty for non-symlinks.
	Target string
}

// ListResult is the result of a ListDirectory callback call.
type ListResult struct {
	// Entries is the list of directory entries in this page.
	Entries []DirEntry

	// NextOffset is the item offset to resume listing from.
	// Nil when there are no more entries.
	NextOffset *int

	// Remaining is the number of entries remaining after NextOffset.
	// 0 when there are no more entries.
	Remaining int
}

// Match represents a search hit from SearchCodebase.
type Match struct {
	// Path is the file path relative to the worktree root.
	Path string `json:"path"`

	// Offset is the byte offset of the match in the file.
	Offset int64 `json:"offset"`

	// Length is the byte length of the matched text.
	Length int `json:"length"`
}

// SearchResult is the result of a SearchCodebase callback call.
type SearchResult struct {
	// Matches is the list of search hits in this page.
	Matches []Match

	// NextOffset is the item offset to resume searching from.
	// Nil when there are no more matches.
	NextOffset *int

	// HasMore indicates that additional matches exist beyond this page.
	HasMore bool
}

// EditResult is the result of an EditFile callback call.
type EditResult struct {
	// Replacements is the number of occurrences that were replaced.
	Replacements int
}

// WorktreeCallbacks provides callback functions for file operations on a
// worktree. Write, delete, move, copy, symlink, and chmod operations
// automatically stage changes when backed by a git worktree.
type WorktreeCallbacks struct {
	// ReadFile reads content from a file starting at the given byte offset,
	// returning up to limit bytes. Pass limit=-1 to read the entire file.
	ReadFile func(ctx context.Context, path string, offset int64, limit int) (ReadResult, error)

	// WriteFile creates or overwrites a file with the given content and mode.
	WriteFile func(ctx context.Context, path, content string, mode os.FileMode) error

	// DeleteFile removes a file.
	DeleteFile func(ctx context.Context, path string) error

	// MoveFile moves/renames a file from src to dst.
	MoveFile func(ctx context.Context, src, dst string) error

	// CopyFile copies a file from src to dst.
	CopyFile func(ctx context.Context, src, dst string) error

	// CreateSymlink creates a symbolic link at path pointing to target.
	CreateSymlink func(ctx context.Context, path, target string) error

	// Chmod changes the file mode of the file at path.
	Chmod func(ctx context.Context, path string, mode os.FileMode) error

	// ListDirectory lists directory entries with metadata. The filter
	// parameter supports glob patterns (with * wildcards) or exact name
	// matching. Results are paginated by offset and limit.
	ListDirectory func(ctx context.Context, path, filter string, offset, limit int) (ListResult, error)

	// EditFile replaces occurrences of oldString with newString in the file
	// at path. When replaceAll is false, oldString must appear exactly once
	// in the file. Changes are automatically staged.
	EditFile func(ctx context.Context, path, oldString, newString string, replaceAll bool) (EditResult, error)

	// SearchCodebase searches for a regex pattern across files. The filter
	// parameter supports glob patterns (with * wildcards) or exact filename
	// matching. Results are paginated by offset and limit. Match results
	// contain byte offsets into files (no content).
	SearchCodebase func(ctx context.Context, path, pattern, filter string, offset, limit int) (SearchResult, error)
}
