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
		ReadFile: func(ctx context.Context, path string, offset int64, limit int) (ReadResult, error) {
			// Read file from worktree with offset/limit windowing
		},
		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error {
			// Write file to worktree and stage it
		},
		// ... other callbacks (DeleteFile, MoveFile, CopyFile, etc.)
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

# Result Validators

ResultValidator inspects a result an agent submitted via its terminal submit
tool and returns findings describing why it is not acceptable; an empty return
accepts the result. Executors run every registered validator concurrently
(ValidateResult) when the model calls the submit tool, and reject the
submission back to the model (RejectionResult) when any validator returns
findings — keeping the agent loop going until a submission passes:

	validator := func(ctx context.Context, r Report, reasoning string) ([]callbacks.Finding, error) {
		if r.Answer == "" {
			return []callbacks.Finding{{
				Kind:       callbacks.FindingKindReview,
				Identifier: "empty-answer",
				Details:    "the answer field is empty",
			}}, nil
		}
		return nil, nil
	}

# History Callbacks

HistoryCallbacks provides access to commit history and file diffs:

	cb := callbacks.HistoryCallbacks{
		ListCommits: func(ctx context.Context, offset, limit int) (CommitListResult, error) {
			// List commits since base ref with pagination
		},
		GetFileDiff: func(ctx context.Context, path, start, end string, offset int64, limit int) (FileDiffResult, error) {
			// Return paginated unified diff for a file over a commit range
		},
	}
*/
package callbacks
