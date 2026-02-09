/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package callbacks provides lightweight callback types for AI agent tool operations.

This package contains the callback interfaces and data types used by tools
without importing AI SDK dependencies. Packages that need to provide callback
implementations (like clonemanager and changemanager) can import this package
without pulling in anthropic-sdk-go or google.golang.org/genai.

For the full tool provider pattern with AI SDK integration, import the parent
toolcall package instead.

# Worktree Callbacks

WorktreeCallbacks provides file operations on a git worktree:

	cb := callbacks.WorktreeCallbacks{
		ReadFile: func(ctx context.Context, path string) (string, error) {
			// Read file from worktree
		},
		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error {
			// Write file to worktree and stage it
		},
		// ... other callbacks
	}

# Finding Callbacks

FindingCallbacks provides access to CI failure information:

	cb := callbacks.FindingCallbacks{
		Findings: findings, // List of findings for lookup by extensions
		GetDetails: func(ctx context.Context, kind FindingKind, id string) (string, error) {
			// Return pre-fetched finding details
		},
		GetLogs: func(ctx context.Context, kind FindingKind, id string) (string, error) {
			// Fetch and return logs for the finding
		},
	}
*/
package callbacks
