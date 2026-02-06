/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package toolcall defines callback interfaces for AI agent file and finding operations.
//
// This package contains pure type definitions with no external dependencies, allowing
// agent implementations to depend on these types without pulling in heavy dependencies
// like git clients or GitHub APIs.
//
// # WorktreeTools
//
// WorktreeTools provides callbacks for file operations on a git worktree:
//
//	tools := toolcall.WorktreeTools{
//		ReadFile: func(ctx context.Context, path string) (string, error) { ... },
//		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error { ... },
//		// ...
//	}
//
// Factory functions that create WorktreeTools from actual git worktrees are provided
// by the clonemanager package.
//
// # FindingTools
//
// FindingTools provides callbacks for retrieving CI/CD finding information:
//
//	tools := toolcall.FindingTools{
//		GetDetails: func(ctx context.Context, kind FindingKind, id string) (string, error) { ... },
//		GetLogs: func(ctx context.Context, kind FindingKind, id string) (string, error) { ... },
//	}
//
// Factory functions that create FindingTools from GitHub sessions are provided
// by the changemanager package.
package toolcall
