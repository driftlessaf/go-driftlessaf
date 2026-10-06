/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sort"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

// HistoryTools wraps a base tools type and adds history callbacks.
type HistoryTools[T any] struct {
	base T
	callbacks.HistoryCallbacks
}

// NewHistoryTools creates a HistoryTools wrapping the given base tools.
func NewHistoryTools[T any](base T, cb callbacks.HistoryCallbacks) HistoryTools[T] {
	return HistoryTools[T]{base: base, HistoryCallbacks: cb}
}

// historyToolsProvider wraps a base ToolProvider and adds history tools.
type historyToolsProvider[Resp, T any] struct {
	baseProvider ToolProvider[Resp, T]
}

var _ ToolProvider[any, HistoryTools[any]] = (*historyToolsProvider[any, any])(nil)

// NewHistoryToolsProvider creates a provider that adds history tools
// (list_commits, get_file_diff) on top of the base provider's tools.
func NewHistoryToolsProvider[Resp, T any](base ToolProvider[Resp, T]) ToolProvider[Resp, HistoryTools[T]] {
	return historyToolsProvider[Resp, T]{baseProvider: base}
}

func (p historyToolsProvider[Resp, T]) Tools(ctx context.Context, cb HistoryTools[T]) (map[string]Tool[Resp], error) {
	tools, err := p.baseProvider.Tools(ctx, cb.base)
	if err != nil {
		return nil, err
	}
	maps.Copy(tools, historyToolDefs[Resp](cb.HistoryCallbacks))
	return tools, nil
}

const (
	// maxListCommitsLimit is the maximum number of commits that can be
	// returned in a single list_commits call.
	maxListCommitsLimit = 100

	// defaultListCommitsLimit is the number of commits list_commits returns
	// when the caller passes no limit.
	defaultListCommitsLimit = 10

	// maxListCommitsFiles is the maximum number of files listed per commit.
	// A merge from main can touch tens of thousands of files.
	maxListCommitsFiles = 200

	// maxListCommitsMessageBytes is the maximum size of a commit message in
	// a list_commits response.
	maxListCommitsMessageBytes = 4000

	// maxListCommitsBytes is the maximum size of an encoded list_commits
	// response.
	maxListCommitsBytes = 256000

	// maxFileDiffLimit is the maximum number of bytes that can be returned
	// in a single get_file_diff call.
	maxFileDiffLimit = 100000
)

func historyToolDefs[Resp any](cb callbacks.HistoryCallbacks) map[string]Tool[Resp] {
	return map[string]Tool[Resp]{
		"list_commits": {
			Def: Definition{
				Name: "list_commits",
				Description: fmt.Sprintf("List commits since the base branch in reverse chronological order. Each commit includes its changed files with diff sizes in bytes, allowing you to decide which diffs to fetch with get_file_diff. "+
					"The response is at most limit_bytes (%d) bytes, and truncated is true when anything is cut. "+
					"Each commit lists at most %d files and %d bytes of message. files_total counts all of its files, files_omitted counts the files not listed, and message_truncated marks a cut message. "+
					"To list the omitted files of a commit, call list_commits again with offset at that commit, limit 1, and files_offset set to its files_next_offset. "+
					"To read the change to a file you know, call get_file_diff(path, start, end) with start at the next older commit and end at the commit. "+
					"When the byte bound drops trailing commits, next_offset points at the first dropped commit.", maxListCommitsBytes, maxListCommitsFiles, maxListCommitsMessageBytes),
				Parameters: []Parameter{{
					Name: "offset", Type: "integer", Description: "Number of commits to skip (default: 0)", Required: false,
				}, {
					Name: "limit", Type: "integer", Description: fmt.Sprintf("Maximum commits to return (default: %d, max: %d)", defaultListCommitsLimit, maxListCommitsLimit), Required: false,
				}, {
					Name: "files_offset", Type: "integer", Description: "Number of files to skip in each commit's file list (default: 0)", Required: false,
				}},
				Annotations: &ToolAnnotations{
					ReadOnly:    true,
					Destructive: new(false),
					Idempotent:  true,
					OpenWorld:   new(false),
				},
			},
			Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
				offset, errResp := OptionalParam[int](call, "offset", 0)
				if errResp != nil {
					return errResp
				}
				limit, errResp := OptionalParam[int](call, "limit", defaultListCommitsLimit)
				if errResp != nil {
					return errResp
				}
				if limit > maxListCommitsLimit {
					return params.Error("limit %d exceeds maximum of %d", limit, maxListCommitsLimit)
				}
				filesOffset, errResp := OptionalParam[int](call, "files_offset", 0)
				if errResp != nil {
					return errResp
				}
				if filesOffset < 0 {
					return params.Error("files_offset %d must not be negative", filesOffset)
				}

				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"offset": offset, "limit": limit, "files_offset": filesOffset})

				result, err := cb.ListCommits(ctx, offset, limit)
				if err != nil {
					clog.ErrorContext(ctx, "Failed to list commits", "error", err)
					resp := params.ErrorWithContext(err, map[string]any{"offset": offset, "limit": limit})
					tc.Complete(resp, err)
					return resp
				}

				resp := formatCommitListResult(result, offset, filesOffset)
				tc.Complete(resp, nil)
				return resp
			},
		},
		"get_file_diff": {
			Def: Definition{
				Name:        "get_file_diff",
				Description: "Get the unified diff for a specific file over a commit range. Omit start for base ref, omit end for HEAD. The diff shows what changed after start up through end.",
				Parameters: []Parameter{{
					Name: "path", Type: "string", Description: "File path (relative to repository root)", Required: true,
				}, {
					Name: "start", Type: "string", Description: "Start commit SHA (exclusive). Omit for base ref.", Required: false,
				}, {
					Name: "end", Type: "string", Description: "End commit SHA (inclusive). Omit for HEAD.", Required: false,
				}, {
					Name: "offset", Type: "integer", Description: "Byte offset into the diff to start reading from (default: 0)", Required: false,
				}, {
					Name: "limit", Type: "integer", Description: fmt.Sprintf("Maximum bytes of diff to return (default: 20000, max: %d)", maxFileDiffLimit), Required: false,
				}},
				Annotations: &ToolAnnotations{
					ReadOnly:    true,
					Destructive: new(false),
					Idempotent:  true,
					OpenWorld:   new(false),
				},
			},
			Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
				path, errResp := Param[string](call, trace, "path")
				if errResp != nil {
					return errResp
				}

				start, errResp := OptionalParam[string](call, "start", "")
				if errResp != nil {
					return errResp
				}
				end, errResp := OptionalParam[string](call, "end", "")
				if errResp != nil {
					return errResp
				}
				offset, errResp := OptionalParam[int64](call, "offset", 0)
				if errResp != nil {
					return errResp
				}
				limit, errResp := OptionalParam[int](call, "limit", 20000)
				if errResp != nil {
					return errResp
				}
				if limit > maxFileDiffLimit {
					return params.Error("limit %d exceeds maximum of %d", limit, maxFileDiffLimit)
				}

				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"path": path, "start": start, "end": end, "offset": offset, "limit": limit})

				result, err := cb.GetFileDiff(ctx, path, start, end, offset, limit)
				if err != nil {
					return completeError(ctx, tc, "Failed to get file diff", err, "path", path)
				}

				resp := map[string]any{
					"diff": result.Diff,
				}
				if result.NextOffset != nil {
					resp["next_offset"] = *result.NextOffset
				}
				resp["remaining"] = result.Remaining
				tc.Complete(resp, nil)
				return resp
			},
		},
	}
}

// formatCommitListResult encodes a page of commits within
// maxListCommitsBytes. It drops whole trailing commits first, so next_offset
// never skips or repeats a commit, and only cuts the file list of a page's
// sole commit.
func formatCommitListResult(result callbacks.CommitListResult, offset, filesOffset int) map[string]any {
	commits := make([]map[string]any, 0, len(result.Commits))
	truncated := false
	for _, c := range result.Commits {
		commit, cut := formatCommit(c, filesOffset, maxListCommitsFiles)
		truncated = truncated || cut
		commits = append(commits, commit)
	}

	resp := map[string]any{
		"commits":     commits,
		"total":       result.Total,
		"limit_bytes": maxListCommitsBytes,
	}
	if result.NextOffset != nil {
		resp["next_offset"] = *result.NextOffset
	}
	if truncated {
		resp["truncated"] = true
	}
	if encodedSize(resp) <= maxListCommitsBytes {
		return resp
	}

	// The base size assumes truncated and the largest next_offset, so the
	// kept prefix stays within the bound whatever those fields hold.
	resp["truncated"] = true
	resp["commits"] = []map[string]any{}
	resp["next_offset"] = max(offset+len(commits), result.Total)
	size := encodedSize(resp)
	kept := 0
	for i, commit := range commits {
		next := encodedSize(commit)
		if i > 0 {
			next++ // comma
		}
		if size+next > maxListCommitsBytes {
			break
		}
		size += next
		kept++
	}
	kept = min(max(kept, 1), len(commits))
	resp["commits"] = commits[:kept]
	switch {
	case kept < len(commits):
		resp["next_offset"] = offset + kept
	case result.NextOffset != nil:
		resp["next_offset"] = *result.NextOffset
	default:
		delete(resp, "next_offset")
	}

	if kept == 1 && encodedSize(resp) > maxListCommitsBytes {
		c := result.Commits[0]
		// The message cap keeps a commit with no files within the bound, so
		// sort.Search finds at least zero files that fit.
		fits := sort.Search(maxListCommitsFiles+1, func(n int) bool {
			commit, _ := formatCommit(c, filesOffset, n)
			resp["commits"] = []map[string]any{commit}
			return encodedSize(resp) > maxListCommitsBytes
		}) - 1
		commit, _ := formatCommit(c, filesOffset, max(fits, 0))
		resp["commits"] = []map[string]any{commit}
	}
	return resp
}

// formatCommit encodes one commit with at most maxFiles files, starting at
// filesOffset. It reports whether it cut the message or the file list.
func formatCommit(c callbacks.CommitInfo, filesOffset, maxFiles int) (map[string]any, bool) {
	commit := map[string]any{
		"sha":         c.SHA,
		"message":     c.Message,
		"files_total": len(c.Files),
	}
	cut := false
	if len(c.Message) > maxListCommitsMessageBytes {
		commit["message"] = truncateUTF8(c.Message, maxListCommitsMessageBytes)
		commit["message_truncated"] = true
		cut = true
	}

	start := min(filesOffset, len(c.Files))
	end := min(start+maxFiles, len(c.Files))
	files := make([]map[string]any, 0, end-start)
	for _, f := range c.Files[start:end] {
		file := map[string]any{
			"path":      f.Path,
			"type":      f.Type,
			"diff_size": f.DiffSize,
		}
		if f.OldPath != "" {
			file["old_path"] = f.OldPath
		}
		files = append(files, file)
	}
	commit["files"] = files
	if omitted := len(c.Files) - len(files); omitted > 0 {
		commit["files_omitted"] = omitted
	}
	if end < len(c.Files) {
		commit["files_next_offset"] = end
		cut = true
	}
	return commit, cut
}

// encodedSize returns the JSON size of v. An encoding failure reports the
// largest size, so the caller trims rather than sends an unmeasured response.
func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return math.MaxInt
	}
	return len(b)
}

// truncateUTF8 returns the longest prefix of s that is at most maxBytes bytes
// and does not split a multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
