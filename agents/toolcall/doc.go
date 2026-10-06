/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package toolcall defines composable tool providers for AI agents.
//
// This package provides a layered tool composition system for AI agent file, finding,
// and history operations. Tools are composed using generics:
// Empty -> Worktree -> Finding -> History.
//
// Callback types (WorktreeCallbacks, FindingCallbacks, etc.) are defined in the
// toolcall/callbacks subpackage. This separation allows packages that only need
// callback types to avoid importing AI SDK dependencies.
//
// # Tool Composition
//
// Tools are composed by wrapping callback structs in generic wrappers:
//
//	// Callbacks hold the actual implementation functions
//	wt := callbacks.WorktreeCallbacks{
//		ReadFile: func(ctx context.Context, path string) (string, error) { ... },
//		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error { ... },
//	}
//	fc := callbacks.FindingCallbacks{
//		GetDetails: func(ctx context.Context, kind callbacks.FindingKind, id string) (string, error) { ... },
//		GetLogs: func(ctx context.Context, kind callbacks.FindingKind, id string) (string, error) { ... },
//	}
//
//	hc := callbacks.HistoryCallbacks{
//		ListCommits: func(ctx context.Context, offset, limit int) (callbacks.CommitListResult, error) { ... },
//		GetFileDiff: func(ctx context.Context, path, start, end string, offset int64, limit int) (callbacks.FileDiffResult, error) { ... },
//	}
//
//	// Compose tools: Empty -> Worktree -> Finding -> History
//	tools := toolcall.NewHistoryTools(
//		toolcall.NewFindingTools(
//			toolcall.NewWorktreeTools(toolcall.EmptyTools{}, wt),
//			fc,
//		),
//		hc,
//	)
//
// # Tool Providers
//
// Providers generate tool definitions for specific AI backends (Claude, Gemini):
//
//	provider := toolcall.NewFindingToolsProvider[*Response, toolcall.WorktreeTools[toolcall.EmptyTools]](
//		toolcall.NewWorktreeToolsProvider[*Response, toolcall.EmptyTools](
//			toolcall.NewEmptyToolsProvider[*Response](),
//		),
//	)
//
//	claudeTools := provider.ClaudeTools(tools)
//	googleTools := provider.GoogleTools(tools)
//
// # Result Bounds
//
// A tool result goes into the model context, so each tool that can return
// unbounded content caps it and tells the model how to fetch the rest.
//
// list_commits returns 10 commits by default and 100 at most. A merge from the
// base branch can touch tens of thousands of files, so the response also has
// these bounds:
//   - Each commit lists at most 200 files and 4,000 bytes of message.
//   - The encoded response is at most 256,000 bytes. The limit_bytes field
//     reports this value.
//
// The response sets truncated when anything is cut. Each commit carries
// files_total (all its files), files_omitted (files not listed), and
// message_truncated. When the byte bound drops trailing commits, next_offset
// points at the first dropped commit, so a following page neither skips nor
// repeats a commit. The bound cuts the file list only for a page of one
// commit.
//
// The files_offset parameter skips files in every commit of the page. To list
// the omitted files of one commit, call list_commits with offset at that
// commit, limit 1, and files_offset set to its files_next_offset. To read the
// change to a known file, call get_file_diff(path, start, end).
//
// search_finding_logs returns at most 100 matches per page. Callers page past
// that cap with skip. Each match is an offset and length pointer with no log
// content, so the encoded response is at most 10,000 bytes plus the kind and
// identifier arguments it echoes. That figure allows for a 512-byte pattern
// whose every byte JSON escapes to six.
//
// # Callback Sources
//
// Factory functions for callbacks are provided by other packages:
//   - WorktreeCallbacks: clonemanager.WorktreeCallbacks(worktree)
//   - FindingCallbacks: session.FindingCallbacks()
//   - HistoryCallbacks: clonemanager.HistoryCallbacks(repo, baseCommit)
package toolcall
