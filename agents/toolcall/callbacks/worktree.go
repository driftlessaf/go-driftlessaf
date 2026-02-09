/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import (
	"context"
	"os"
)

// Match represents a search result from SearchCodebase.
type Match struct {
	// Path is the file path relative to the worktree root
	Path string `json:"path"`

	// Line is the line number (1-based)
	Line int `json:"line"`

	// Content is the matching line content
	Content string `json:"content"`
}

// WorktreeCallbacks provides callback functions for file operations on a git worktree.
// Write and delete operations automatically stage changes to the git index.
type WorktreeCallbacks struct {
	// ReadFile reads a file from the worktree.
	ReadFile func(ctx context.Context, path string) (content string, err error)

	// WriteFile writes content to a file in the worktree.
	WriteFile func(ctx context.Context, path, content string, mode os.FileMode) error

	// DeleteFile removes a file from the worktree and auto-stages the deletion.
	DeleteFile func(ctx context.Context, path string) error

	// ListDirectory lists the contents of a directory in the worktree.
	ListDirectory func(ctx context.Context, path string) (entries []string, err error)

	// SearchCodebase searches for a pattern in the worktree.
	SearchCodebase func(ctx context.Context, pattern string) (matches []Match, err error)
}
