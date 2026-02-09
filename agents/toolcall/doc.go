/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package toolcall defines composable tool providers for AI agents.
//
// This package provides a layered tool composition system for AI agent file and finding
// operations. Tools are composed using generics: Empty -> Worktree -> Finding.
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
//	// Compose tools: Empty -> Worktree -> Finding
//	tools := toolcall.NewFindingTools(
//		toolcall.NewWorktreeTools(toolcall.EmptyTools{}, wt),
//		fc,
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
package toolcall
