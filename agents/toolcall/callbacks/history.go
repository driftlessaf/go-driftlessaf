/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks

import "context"

// CommitFile describes a file changed in a single commit.
type CommitFile struct {
	// Path is the file path relative to the repository root.
	Path string

	// OldPath is the previous path for renamed files. Empty otherwise.
	OldPath string

	// Type is the change type: "added", "modified", "deleted", or "renamed".
	Type string

	// DiffSize is the size of this file's unified diff in bytes.
	DiffSize int
}

// CommitInfo describes a single commit in the history.
type CommitInfo struct {
	// SHA is the short (7-character) commit hash.
	SHA string

	// Message is the full commit message.
	Message string

	// Files is the list of files changed in this commit.
	Files []CommitFile
}

// CommitListResult is the result of a ListCommits callback call.
type CommitListResult struct {
	// Commits is the list of commits in this page.
	Commits []CommitInfo

	// NextOffset is the item offset to resume listing from.
	// Nil when there are no more commits.
	NextOffset *int

	// Total is the total number of commits in the range.
	Total int
}

// FileDiffResult is the result of a GetFileDiff callback call.
type FileDiffResult struct {
	// Diff is the unified diff text for this page.
	Diff string

	// NextOffset is the byte offset to resume reading from.
	// Nil when the entire diff has been returned.
	NextOffset *int64

	// Remaining is the number of bytes remaining after NextOffset.
	Remaining int64
}

// HistoryCallbacks provides callback functions for reading commit history
// relative to a fixed base ref determined at construction time.
type HistoryCallbacks struct {
	// ListCommits lists commits from the base ref to HEAD in reverse
	// chronological order. Each commit includes its changed files with
	// diff sizes. Results are paginated by offset and limit.
	ListCommits func(ctx context.Context, offset, limit int) (CommitListResult, error)

	// GetFileDiff returns the unified diff for a single file over a
	// commit range. Pass empty start for base ref, empty end for HEAD.
	// Results are paginated by byte offset and limit.
	GetFileDiff func(ctx context.Context, path, start, end string, offset int64, limit int) (FileDiffResult, error)
}
