/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"fmt"
	"maps"

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

func (p historyToolsProvider[Resp, T]) Tools(cb HistoryTools[T]) map[string]Tool[Resp] {
	tools := p.baseProvider.Tools(cb.base)
	maps.Copy(tools, historyToolDefs[Resp](cb.HistoryCallbacks))
	return tools
}

const (
	// maxListCommitsLimit is the maximum number of commits that can be
	// returned in a single list_commits call.
	maxListCommitsLimit = 100

	// maxFileDiffLimit is the maximum number of bytes that can be returned
	// in a single get_file_diff call.
	maxFileDiffLimit = 100000
)

func historyToolDefs[Resp any](cb callbacks.HistoryCallbacks) map[string]Tool[Resp] {
	return map[string]Tool[Resp]{
		"list_commits": {
			Def: Definition{
				Name:        "list_commits",
				Description: "List commits since the base branch in reverse chronological order. Each commit includes its changed files with diff sizes in bytes, allowing you to decide which diffs to fetch with get_file_diff.",
				Parameters: []Parameter{{
					Name: "offset", Type: "integer", Description: "Number of commits to skip (default: 0)", Required: false,
				}, {
					Name: "limit", Type: "integer", Description: fmt.Sprintf("Maximum commits to return (default: 20, max: %d)", maxListCommitsLimit), Required: false,
				}},
			},
			Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
				log := clog.FromContext(ctx)

				offset, errResp := OptionalParam[int](call, "offset", 0)
				if errResp != nil {
					return errResp
				}
				limit, errResp := OptionalParam[int](call, "limit", 20)
				if errResp != nil {
					return errResp
				}
				if limit > maxListCommitsLimit {
					return params.Error("limit %d exceeds maximum of %d", limit, maxListCommitsLimit)
				}

				tc := trace.StartToolCall(call.ID, call.Name, map[string]any{"offset": offset, "limit": limit})

				result, err := cb.ListCommits(ctx, offset, limit)
				if err != nil {
					log.With("error", err).Error("Failed to list commits")
					resp := params.ErrorWithContext(err, map[string]any{"offset": offset, "limit": limit})
					tc.Complete(resp, err)
					return resp
				}

				resp := formatCommitListResult(result)
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
			},
			Handler: func(ctx context.Context, call ToolCall, trace *agenttrace.Trace[Resp], _ *Resp) map[string]any {
				log := clog.FromContext(ctx)

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
					log.With("path", path).With("error", err).Error("Failed to get file diff")
					resp := params.ErrorWithContext(err, map[string]any{"path": path})
					tc.Complete(resp, err)
					return resp
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

func formatCommitListResult(result callbacks.CommitListResult) map[string]any {
	commits := make([]map[string]any, 0, len(result.Commits))
	for _, c := range result.Commits {
		commit := map[string]any{
			"sha":     c.SHA,
			"message": c.Message,
		}

		files := make([]map[string]any, 0, len(c.Files))
		for _, f := range c.Files {
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

		commits = append(commits, commit)
	}

	resp := map[string]any{
		"commits": commits,
		"total":   result.Total,
	}
	if result.NextOffset != nil {
		resp["next_offset"] = *result.NextOffset
	}
	return resp
}
