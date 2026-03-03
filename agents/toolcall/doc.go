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
// # Callback Sources
//
// Factory functions for callbacks are provided by other packages:
//   - WorktreeCallbacks: clonemanager.WorktreeCallbacks(worktree)
//   - FindingCallbacks: session.FindingCallbacks()
//   - HistoryCallbacks: clonemanager.HistoryCallbacks(repo, baseCommit)
package toolcall
