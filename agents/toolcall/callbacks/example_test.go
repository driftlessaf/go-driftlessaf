/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks_test

import (
	"context"
	"fmt"
	"os"
	"strings"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

// ExampleWorktreeCallbacks demonstrates how to use WorktreeCallbacks for file operations.
func ExampleWorktreeCallbacks() {
	ctx := context.Background()

	cb := callbacks.WorktreeCallbacks{
		ReadFile: func(ctx context.Context, path string, offset int64, limit int) (callbacks.ReadResult, error) {
			return callbacks.ReadResult{
				Content:    "file content",
				NextOffset: nil,
				Remaining:  0,
			}, nil
		},
		WriteFile: func(ctx context.Context, path, content string, mode os.FileMode) error {
			fmt.Printf("Writing to %s\n", path)
			return nil
		},
		DeleteFile: func(ctx context.Context, path string) error {
			fmt.Printf("Deleting %s\n", path)
			return nil
		},
		MoveFile: func(ctx context.Context, src, dst string) error {
			fmt.Printf("Moving %s to %s\n", src, dst)
			return nil
		},
		CopyFile: func(ctx context.Context, src, dst string) error {
			fmt.Printf("Copying %s to %s\n", src, dst)
			return nil
		},
		CreateSymlink: func(ctx context.Context, path, target string) error {
			fmt.Printf("Creating symlink %s -> %s\n", path, target)
			return nil
		},
		Chmod: func(ctx context.Context, path string, mode os.FileMode) error {
			fmt.Printf("Changing mode of %s to %o\n", path, mode)
			return nil
		},
		ListDirectory: func(ctx context.Context, path, filter string, offset, limit int) (callbacks.ListResult, error) {
			return callbacks.ListResult{
				Entries: []callbacks.DirEntry{{
					Name: "example.go",
					Size: 1024,
					Mode: 0644,
					Type: "file",
				}},
				NextOffset: nil,
				Remaining:  0,
			}, nil
		},
		EditFile: func(ctx context.Context, path, oldString, newString string, replaceAll bool) (callbacks.EditResult, error) {
			return callbacks.EditResult{
				Replacements: 1,
			}, nil
		},
		SearchCodebase: func(ctx context.Context, path, pattern, filter string, offset, limit int) (callbacks.SearchResult, error) {
			return callbacks.SearchResult{
				Matches: []callbacks.Match{{
					Path:   "example.go",
					Offset: 100,
					Length: 10,
				}},
				NextOffset: nil,
				HasMore:    false,
			}, nil
		},
	}

	// Use the callbacks
	result, _ := cb.ReadFile(ctx, "example.go", 0, -1)
	fmt.Println(result.Content)

	_ = cb.WriteFile(ctx, "new.go", "package main", 0644)
	_ = cb.DeleteFile(ctx, "old.go")
	_ = cb.MoveFile(ctx, "src.go", "dst.go")
	_ = cb.CopyFile(ctx, "original.go", "copy.go")
	_ = cb.CreateSymlink(ctx, "link", "target")
	_ = cb.Chmod(ctx, "script.sh", 0755)

	listResult, _ := cb.ListDirectory(ctx, ".", "", 0, 50)
	fmt.Printf("Found %d entries\n", len(listResult.Entries))

	editResult, _ := cb.EditFile(ctx, "file.go", "old", "new", false)
	fmt.Printf("Replaced %d occurrences\n", editResult.Replacements)

	searchResult, _ := cb.SearchCodebase(ctx, ".", "pattern", "*.go", 0, 50)
	fmt.Printf("Found %d matches\n", len(searchResult.Matches))

	// Output:
	// file content
	// Writing to new.go
	// Deleting old.go
	// Moving src.go to dst.go
	// Copying original.go to copy.go
	// Creating symlink link -> target
	// Changing mode of script.sh to 755
	// Found 1 entries
	// Replaced 1 occurrences
	// Found 1 matches
}

// ExampleFindingCallbacks demonstrates how to use FindingCallbacks for accessing CI failure information.
func ExampleFindingCallbacks() {
	ctx := context.Background()

	findings := []callbacks.Finding{{
		Kind:       callbacks.FindingKindCICheck,
		Identifier: "test-job-123",
		Details:    "Test failed: assertion error",
		DetailsURL: "https://example.com/job/123",
	}, {
		Kind:       callbacks.FindingKindReview,
		Identifier: "review-456",
		Details:    "Code review feedback",
		DetailsURL: "https://example.com/review/456",
	}}

	cb := callbacks.FindingCallbacks{
		Findings: findings,
		GetDetails: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			for _, f := range findings {
				if f.Kind == kind && f.Identifier == identifier {
					return f.Details, nil
				}
			}
			return "", fmt.Errorf("finding not found")
		},
		GetLogs: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			return "log output for " + identifier, nil
		},
	}

	// Check if callbacks are available
	fmt.Printf("Has GetDetails: %v\n", cb.HasGetDetails())
	fmt.Printf("Has GetLogs: %v\n", cb.HasGetLogs())

	// Look up a finding
	finding := cb.GetFinding(callbacks.FindingKindCICheck, "test-job-123")
	if finding != nil {
		fmt.Printf("Found: %s\n", finding.Identifier)
	}

	// Get details and logs
	details, _ := cb.GetDetails(ctx, callbacks.FindingKindCICheck, "test-job-123")
	fmt.Println(details)

	logs, _ := cb.GetLogs(ctx, callbacks.FindingKindCICheck, "test-job-123")
	fmt.Println(logs)

	// Output:
	// Has GetDetails: true
	// Has GetLogs: true
	// Found: test-job-123
	// Test failed: assertion error
	// log output for test-job-123
}

// ExampleHistoryCallbacks demonstrates how to use HistoryCallbacks for accessing commit history.
func ExampleHistoryCallbacks() {
	ctx := context.Background()

	cb := callbacks.HistoryCallbacks{
		ListCommits: func(ctx context.Context, offset, limit int) (callbacks.CommitListResult, error) {
			return callbacks.CommitListResult{
				Commits: []callbacks.CommitInfo{{
					SHA:     "abc1234",
					Message: "feat: add new feature",
					Files: []callbacks.CommitFile{{
						Path:     "main.go",
						Type:     "modified",
						DiffSize: 256,
					}},
				}},
				NextOffset: nil,
				Total:      1,
			}, nil
		},
		GetFileDiff: func(ctx context.Context, path, start, end string, offset int64, limit int) (callbacks.FileDiffResult, error) {
			return callbacks.FileDiffResult{
				Diff:       "diff --git a/main.go b/main.go\n......\n",
				NextOffset: nil,
				Remaining:  0,
			}, nil
		},
	}

	// List commits
	result, _ := cb.ListCommits(ctx, 0, 20)
	fmt.Printf("Total commits: %d\n", result.Total)
	if len(result.Commits) > 0 {
		fmt.Printf("First commit: %s\n", result.Commits[0].SHA)
		fmt.Printf("Message: %s\n", result.Commits[0].Message)
		if len(result.Commits[0].Files) > 0 {
			fmt.Printf("Changed file: %s (%s)\n", result.Commits[0].Files[0].Path, result.Commits[0].Files[0].Type)
		}
	}

	// Get file diff
	diffResult, _ := cb.GetFileDiff(ctx, "main.go", "", "", 0, 20000)
	fmt.Printf("Diff length: %d bytes\n", len(diffResult.Diff))

	// Output:
	// Total commits: 1
	// First commit: abc1234
	// Message: feat: add new feature
	// Changed file: main.go (modified)
	// Diff length: 38 bytes
}

// ExampleFindingKind demonstrates the FindingKind constants.
func ExampleFindingKind() {
	ciCheck := callbacks.FindingKindCICheck
	review := callbacks.FindingKindReview

	fmt.Printf("CI Check: %s\n", ciCheck)
	fmt.Printf("Review: %s\n", review)

	// Output:
	// CI Check: ciCheck
	// Review: review
}

// ExampleFinding demonstrates the Finding type.
func ExampleFinding() {
	finding := callbacks.Finding{
		Kind:       callbacks.FindingKindCICheck,
		Identifier: "job-123",
		Details:    "Build failed",
		DetailsURL: "https://example.com/job/123",
	}

	fmt.Printf("Kind: %s\n", finding.Kind)
	fmt.Printf("ID: %s\n", finding.Identifier)
	fmt.Printf("Details: %s\n", finding.Details)

	// Output:
	// Kind: ciCheck
	// ID: job-123
	// Details: Build failed
}

// ExampleReadResult demonstrates the ReadResult type.
func ExampleReadResult() {
	nextOffset := int64(1024)
	result := callbacks.ReadResult{
		Content:    "file content",
		NextOffset: &nextOffset,
		Remaining:  512,
	}

	fmt.Printf("Content: %s\n", result.Content)
	fmt.Printf("Next offset: %d\n", *result.NextOffset)
	fmt.Printf("Remaining: %d bytes\n", result.Remaining)

	// Output:
	// Content: file content
	// Next offset: 1024
	// Remaining: 512 bytes
}

// ExampleDirEntry demonstrates the DirEntry type.
func ExampleDirEntry() {
	entry := callbacks.DirEntry{
		Name:   "example.go",
		Size:   1024,
		Mode:   0644,
		Type:   "file",
		Target: "",
	}

	fmt.Printf("Name: %s\n", entry.Name)
	fmt.Printf("Size: %d bytes\n", entry.Size)
	fmt.Printf("Mode: %o\n", entry.Mode)
	fmt.Printf("Type: %s\n", entry.Type)

	// Output:
	// Name: example.go
	// Size: 1024 bytes
	// Mode: 644
	// Type: file
}

// ExampleListResult demonstrates the ListResult type.
func ExampleListResult() {
	result := callbacks.ListResult{
		Entries: []callbacks.DirEntry{{
			Name: "file1.go",
			Size: 512,
			Mode: 0644,
			Type: "file",
		}, {
			Name: "file2.go",
			Size: 1024,
			Mode: 0644,
			Type: "file",
		}},
		NextOffset: nil,
		Remaining:  0,
	}

	fmt.Printf("Entries: %d\n", len(result.Entries))
	fmt.Printf("Has more: %v\n", result.NextOffset != nil)

	// Output:
	// Entries: 2
	// Has more: false
}

// ExampleMatch demonstrates the Match type.
func ExampleMatch() {
	match := callbacks.Match{
		Path:   "main.go",
		Offset: 256,
		Length: 10,
	}

	fmt.Printf("Path: %s\n", match.Path)
	fmt.Printf("Offset: %d\n", match.Offset)
	fmt.Printf("Length: %d\n", match.Length)

	// Output:
	// Path: main.go
	// Offset: 256
	// Length: 10
}

// ExampleSearchResult demonstrates the SearchResult type.
func ExampleSearchResult() {
	result := callbacks.SearchResult{
		Matches: []callbacks.Match{{
			Path:   "main.go",
			Offset: 100,
			Length: 5,
		}},
		NextOffset: nil,
		HasMore:    false,
	}

	fmt.Printf("Matches: %d\n", len(result.Matches))
	fmt.Printf("Has more: %v\n", result.HasMore)

	// Output:
	// Matches: 1
	// Has more: false
}

// ExampleEditResult demonstrates the EditResult type.
func ExampleEditResult() {
	result := callbacks.EditResult{
		Replacements: 3,
	}

	fmt.Printf("Replacements: %d\n", result.Replacements)

	// Output:
	// Replacements: 3
}

// ExampleCommitFile demonstrates the CommitFile type.
func ExampleCommitFile() {
	file := callbacks.CommitFile{
		Path:     "main.go",
		OldPath:  "",
		Type:     "modified",
		DiffSize: 512,
	}

	fmt.Printf("Path: %s\n", file.Path)
	fmt.Printf("Type: %s\n", file.Type)
	fmt.Printf("Diff size: %d bytes\n", file.DiffSize)

	// Output:
	// Path: main.go
	// Type: modified
	// Diff size: 512 bytes
}

// ExampleCommitInfo demonstrates the CommitInfo type.
func ExampleCommitInfo() {
	commit := callbacks.CommitInfo{
		SHA:     "abc1234",
		Message: "feat: add feature",
		Files: []callbacks.CommitFile{{
			Path:     "main.go",
			Type:     "modified",
			DiffSize: 256,
		}},
	}

	fmt.Printf("SHA: %s\n", commit.SHA)
	fmt.Printf("Message: %s\n", commit.Message)
	fmt.Printf("Files changed: %d\n", len(commit.Files))

	// Output:
	// SHA: abc1234
	// Message: feat: add feature
	// Files changed: 1
}

// ExampleCommitListResult demonstrates the CommitListResult type.
func ExampleCommitListResult() {
	result := callbacks.CommitListResult{
		Commits: []callbacks.CommitInfo{{
			SHA:     "abc1234",
			Message: "feat: add feature",
		}},
		NextOffset: nil,
		Total:      1,
	}

	fmt.Printf("Commits: %d\n", len(result.Commits))
	fmt.Printf("Total: %d\n", result.Total)
	fmt.Printf("Has more: %v\n", result.NextOffset != nil)

	// Output:
	// Commits: 1
	// Total: 1
	// Has more: false
}

// ExampleFileDiffResult demonstrates the FileDiffResult type.
func ExampleFileDiffResult() {
	result := callbacks.FileDiffResult{
		Diff:       "diff --git a/main.go b/main.go\n......\n",
		NextOffset: nil,
		Remaining:  0,
	}

	fmt.Printf("Diff length: %d bytes\n", len(result.Diff))
	fmt.Printf("Has more: %v\n", result.NextOffset != nil)

	// Output:
	// Diff length: 38 bytes
	// Has more: false
}

// ExampleFindingCallbacks_HasGetDetails demonstrates the HasGetDetails method.
func ExampleFindingCallbacks_HasGetDetails() {
	cb := callbacks.FindingCallbacks{
		GetDetails: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			return "details", nil
		},
	}

	fmt.Printf("Has GetDetails: %v\n", cb.HasGetDetails())

	cbEmpty := callbacks.FindingCallbacks{}
	fmt.Printf("Empty has GetDetails: %v\n", cbEmpty.HasGetDetails())

	// Output:
	// Has GetDetails: true
	// Empty has GetDetails: false
}

// ExampleFindingCallbacks_HasGetLogs demonstrates the HasGetLogs method.
func ExampleFindingCallbacks_HasGetLogs() {
	cb := callbacks.FindingCallbacks{
		GetLogs: func(ctx context.Context, kind callbacks.FindingKind, identifier string) (string, error) {
			return "logs", nil
		},
	}

	fmt.Printf("Has GetLogs: %v\n", cb.HasGetLogs())

	cbEmpty := callbacks.FindingCallbacks{}
	fmt.Printf("Empty has GetLogs: %v\n", cbEmpty.HasGetLogs())

	// Output:
	// Has GetLogs: true
	// Empty has GetLogs: false
}

// ExampleFindingCallbacks_GetFinding demonstrates the GetFinding method.
func ExampleFindingCallbacks_GetFinding() {
	cb := callbacks.FindingCallbacks{
		Findings: []callbacks.Finding{{
			Kind:       callbacks.FindingKindCICheck,
			Identifier: "job-123",
			Details:    "Build failed",
		}, {
			Kind:       callbacks.FindingKindReview,
			Identifier: "review-456",
			Details:    "Code review",
		}},
	}

	// Find existing finding
	finding := cb.GetFinding(callbacks.FindingKindCICheck, "job-123")
	if finding != nil {
		fmt.Printf("Found: %s\n", finding.Details)
	}

	// Try to find non-existent finding
	notFound := cb.GetFinding(callbacks.FindingKindCICheck, "job-999")
	fmt.Printf("Not found: %v\n", notFound == nil)

	// Output:
	// Found: Build failed
	// Not found: true
}

// ExampleValidateResult demonstrates running result validators against a
// submitted result and building the rejection returned to the model.
func ExampleValidateResult() {
	ctx := context.Background()

	type report struct {
		Answer string
	}

	// Validators run concurrently; each returns findings describing why the
	// result is unacceptable, or nothing to accept it.
	validators := []callbacks.ResultValidator[report]{
		func(_ context.Context, r report, _ string) ([]callbacks.Finding, error) {
			if r.Answer == "" {
				return []callbacks.Finding{{
					Kind:       callbacks.FindingKindReview,
					Identifier: "empty-answer",
					Details:    "the answer field is empty",
				}}, nil
			}
			return nil, nil
		},
	}

	findings, err := callbacks.ValidateResult(ctx, validators, report{}, "the result is complete")
	if err != nil {
		fmt.Printf("validator failed: %v\n", err)
		return
	}

	if len(findings) > 0 {
		rejection := callbacks.RejectionResult("submit_result", findings)
		fmt.Printf("rejected with %d finding(s)\n", len(findings))
		fmt.Printf("error mentions tool: %v\n", strings.Contains(rejection["error"].(string), "submit_result"))
		return
	}
	fmt.Println("accepted")

	// Output:
	// rejected with 1 finding(s)
	// error mentions tool: true
}
