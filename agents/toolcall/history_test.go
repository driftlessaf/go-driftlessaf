/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
)

// commitPage is the decoded form of a list_commits response.
type commitPage struct {
	Commits []struct {
		SHA              string `json:"sha"`
		Message          string `json:"message"`
		MessageTruncated bool   `json:"message_truncated"`
		FilesTotal       int    `json:"files_total"`
		FilesOmitted     int    `json:"files_omitted"`
		FilesNextOffset  *int   `json:"files_next_offset"`
		Files            []struct {
			Path string `json:"path"`
		} `json:"files"`
	} `json:"commits"`
	Total      int  `json:"total"`
	NextOffset *int `json:"next_offset"`
	LimitBytes int  `json:"limit_bytes"`
	Truncated  bool `json:"truncated"`
	Error      string
}

// fakeHistory serves commits from a slice the way the clonemanager callback
// does: a page of at most limit commits from offset, with NextOffset set while
// commits remain.
type fakeHistory struct {
	commits   []callbacks.CommitInfo
	gotLimits []int
}

func fakeCallbacks(f *fakeHistory) callbacks.HistoryCallbacks {
	return callbacks.HistoryCallbacks{
		ListCommits: func(_ context.Context, offset, limit int) (callbacks.CommitListResult, error) {
			f.gotLimits = append(f.gotLimits, limit)
			start := min(offset, len(f.commits))
			end := min(start+limit, len(f.commits))
			result := callbacks.CommitListResult{
				Commits: f.commits[start:end],
				Total:   len(f.commits),
			}
			if end < len(f.commits) {
				result.NextOffset = new(end)
			}
			return result, nil
		},
	}
}

// makeCommits returns n commits, each with files changed files whose paths are
// padded to pathLen bytes.
func makeCommits(n, files, pathLen int) []callbacks.CommitInfo {
	commits := make([]callbacks.CommitInfo, 0, n)
	for i := range n {
		commits = append(commits, callbacks.CommitInfo{
			SHA:     fmt.Sprintf("%07x", i+1),
			Message: fmt.Sprintf("commit %d", i),
			Files:   makeFiles(files, pathLen),
		})
	}
	return commits
}

func makeFiles(n, pathLen int) []callbacks.CommitFile {
	files := make([]callbacks.CommitFile, 0, n)
	for i := range n {
		name := fmt.Sprintf("/file-%06d.go", i)
		files = append(files, callbacks.CommitFile{
			Path:     strings.Repeat("d", max(pathLen-len(name), 0)) + name,
			Type:     "modified",
			DiffSize: 1234,
		})
	}
	return files
}

// callListCommits runs list_commits through the real tool definition and
// checks the byte bound on every response that returns a page.
func callListCommits(t *testing.T, cb callbacks.HistoryCallbacks, args map[string]any) (map[string]any, commitPage) {
	t.Helper()
	tool, ok := historyToolDefs[string](cb)["list_commits"]
	if !ok {
		t.Fatal("list_commits not registered")
	}
	trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
	resp := tool.Handler(t.Context(), ToolCall{ID: "list-1", Name: "list_commits", Args: args}, trace, nil)

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	var page commitPage
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatalf("json.Unmarshal() = %v", err)
	}
	if msg, ok := resp["error"].(string); ok {
		page.Error = msg
		return resp, page
	}
	if len(b) > maxListCommitsBytes {
		t.Errorf("encoded size: got = %d, want <= %d", len(b), maxListCommitsBytes)
	}
	if page.LimitBytes != maxListCommitsBytes {
		t.Errorf("limit_bytes: got = %d, want = %d", page.LimitBytes, maxListCommitsBytes)
	}
	if len(page.Commits) == 0 && page.Total > 0 {
		t.Errorf("commits: got none of %d, want at least one", page.Total)
	}
	return resp, page
}

func TestListCommitsDefaultLimit(t *testing.T) {
	f := &fakeHistory{commits: makeCommits(25, 1, 20)}
	_, page := callListCommits(t, fakeCallbacks(f), map[string]any{})

	if diff := cmp.Diff([]int{defaultListCommitsLimit}, f.gotLimits); diff != "" {
		t.Errorf("callback limit (-want, +got):\n%s", diff)
	}
	if got := len(page.Commits); got != defaultListCommitsLimit {
		t.Errorf("commits: got = %d, want = %d", got, defaultListCommitsLimit)
	}
	if page.NextOffset == nil || *page.NextOffset != defaultListCommitsLimit {
		t.Errorf("next_offset: got = %v, want = %d", page.NextOffset, defaultListCommitsLimit)
	}
	if page.Truncated {
		t.Error("truncated: got = true, want = false")
	}
}

func TestListCommitsFileBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   int
		pathLen int
		// wantFiles is the number of files listed; -1 means fewer than
		// maxListCommitsFiles, cut by the byte bound.
		wantFiles int
	}{
		{name: "1000 files capped at file limit", files: 1000, pathLen: 40, wantFiles: maxListCommitsFiles},
		{name: "20000 files capped at file limit", files: 20000, pathLen: 40, wantFiles: maxListCommitsFiles},
		{name: "20000 long paths cut by byte bound", files: 20000, pathLen: 2000, wantFiles: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHistory{commits: makeCommits(1, tc.files, tc.pathLen)}
			_, page := callListCommits(t, fakeCallbacks(f), map[string]any{})

			if len(page.Commits) != 1 {
				t.Fatalf("commits: got = %d, want = 1", len(page.Commits))
			}
			c := page.Commits[0]
			listed := len(c.Files)
			switch {
			case tc.wantFiles >= 0 && listed != tc.wantFiles:
				t.Errorf("files: got = %d, want = %d", listed, tc.wantFiles)
			case tc.wantFiles < 0 && (listed == 0 || listed >= maxListCommitsFiles):
				t.Errorf("files: got = %d, want between 1 and %d", listed, maxListCommitsFiles-1)
			}
			if c.FilesTotal != tc.files {
				t.Errorf("files_total: got = %d, want = %d", c.FilesTotal, tc.files)
			}
			if c.FilesOmitted != tc.files-listed {
				t.Errorf("files_omitted: got = %d, want = %d", c.FilesOmitted, tc.files-listed)
			}
			if c.FilesNextOffset == nil || *c.FilesNextOffset != listed {
				t.Errorf("files_next_offset: got = %v, want = %d", c.FilesNextOffset, listed)
			}
			if !page.Truncated {
				t.Error("truncated: got = false, want = true")
			}
		})
	}
}

// TestListCommitsMergeCommitPage covers a page of merge commits from main,
// each touching 16,812 files, which overflows the byte bound.
func TestListCommitsMergeCommitPage(t *testing.T) {
	f := &fakeHistory{commits: makeCommits(10, 16812, 200)}
	_, page := callListCommits(t, fakeCallbacks(f), map[string]any{"limit": float64(10)})

	kept := len(page.Commits)
	if kept == 0 || kept >= 10 {
		t.Fatalf("commits: got = %d, want between 1 and 9", kept)
	}
	if page.NextOffset == nil || *page.NextOffset != kept {
		t.Errorf("next_offset: got = %v, want = %d", page.NextOffset, kept)
	}
	if !page.Truncated {
		t.Error("truncated: got = false, want = true")
	}
	for i, c := range page.Commits {
		if want := f.commits[i].SHA; c.SHA != want {
			t.Errorf("commit %d sha: got = %q, want = %q", i, c.SHA, want)
		}
	}
}

func TestListCommitsPagingVisitsEveryCommitOnce(t *testing.T) {
	mixed := makeCommits(30, 2, 20)
	for i := range mixed {
		if i%4 == 0 {
			mixed[i].Files = makeFiles(16812, 200)
		}
	}

	for _, tc := range []struct {
		name    string
		commits []callbacks.CommitInfo
		limit   int
	}{
		{name: "small commits", commits: makeCommits(23, 2, 20), limit: 5},
		{name: "large commits shorten pages", commits: makeCommits(23, 16812, 200), limit: 10},
		{name: "mixed sizes", commits: mixed, limit: 10},
		{name: "commits too large to pair", commits: makeCommits(5, 20000, 2000), limit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHistory{commits: tc.commits}
			seen := make(map[string]struct{}, len(tc.commits))
			offset := 0
			for range len(tc.commits) + 1 {
				_, page := callListCommits(t, fakeCallbacks(f), map[string]any{"offset": float64(offset), "limit": float64(tc.limit)})
				if page.Error != "" {
					t.Fatalf("offset %d: %s", offset, page.Error)
				}
				if page.Total != len(tc.commits) {
					t.Errorf("total: got = %d, want = %d", page.Total, len(tc.commits))
				}
				for _, c := range page.Commits {
					if _, dup := seen[c.SHA]; dup {
						t.Errorf("duplicate SHA across pages: %s", c.SHA)
					}
					seen[c.SHA] = struct{}{}
				}
				if page.NextOffset == nil {
					break
				}
				if *page.NextOffset != offset+len(page.Commits) {
					t.Fatalf("next_offset: got = %d, want = %d", *page.NextOffset, offset+len(page.Commits))
				}
				offset = *page.NextOffset
			}
			if len(seen) != len(tc.commits) {
				t.Errorf("visited commits: got = %d, want = %d", len(seen), len(tc.commits))
			}
		})
	}
}

func TestListCommitsFilesOffsetVisitsEveryFileOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   int
		pathLen int
	}{
		{name: "file cap pages", files: 1000, pathLen: 40},
		{name: "byte bound pages", files: 3000, pathLen: 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commits := makeCommits(3, 1, 20)
			commits[1].Files = makeFiles(tc.files, tc.pathLen)
			f := &fakeHistory{commits: commits}

			seen := make(map[string]struct{}, tc.files)
			filesOffset := 0
			for range tc.files + 1 {
				_, page := callListCommits(t, fakeCallbacks(f), map[string]any{
					"offset": float64(1), "limit": float64(1), "files_offset": float64(filesOffset),
				})
				if len(page.Commits) != 1 {
					t.Fatalf("commits: got = %d, want = 1", len(page.Commits))
				}
				c := page.Commits[0]
				if c.SHA != commits[1].SHA {
					t.Fatalf("sha: got = %q, want = %q", c.SHA, commits[1].SHA)
				}
				for _, file := range c.Files {
					if _, dup := seen[file.Path]; dup {
						t.Errorf("duplicate path across pages: %s", file.Path)
					}
					seen[file.Path] = struct{}{}
				}
				if c.FilesNextOffset == nil {
					break
				}
				if *c.FilesNextOffset != filesOffset+len(c.Files) {
					t.Fatalf("files_next_offset: got = %d, want = %d", *c.FilesNextOffset, filesOffset+len(c.Files))
				}
				filesOffset = *c.FilesNextOffset
			}
			if len(seen) != tc.files {
				t.Errorf("visited files: got = %d, want = %d", len(seen), tc.files)
			}
		})
	}
}

func TestListCommitsMessageTruncation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		message       string
		wantTruncated bool
	}{
		{name: "two-byte runes", message: strings.Repeat("é", 2500), wantTruncated: true},
		{name: "three-byte runes", message: strings.Repeat("€", 2000), wantTruncated: true},
		{name: "four-byte runes", message: "x" + strings.Repeat("🙂", 1500), wantTruncated: true},
		{name: "at the bound", message: strings.Repeat("a", maxListCommitsMessageBytes), wantTruncated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commits := makeCommits(1, 1, 20)
			commits[0].Message = tc.message
			f := &fakeHistory{commits: commits}
			_, page := callListCommits(t, fakeCallbacks(f), map[string]any{})

			got := page.Commits[0]
			if got.MessageTruncated != tc.wantTruncated {
				t.Errorf("message_truncated: got = %t, want = %t", got.MessageTruncated, tc.wantTruncated)
			}
			if page.Truncated != tc.wantTruncated {
				t.Errorf("truncated: got = %t, want = %t", page.Truncated, tc.wantTruncated)
			}
			if !utf8.ValidString(got.Message) {
				t.Error("message: got invalid UTF-8, want a cut on a rune boundary")
			}
			if len(got.Message) > maxListCommitsMessageBytes || len(got.Message) <= maxListCommitsMessageBytes-utf8.UTFMax {
				t.Errorf("message length: got = %d, want in (%d, %d]", len(got.Message), maxListCommitsMessageBytes-utf8.UTFMax, maxListCommitsMessageBytes)
			}
			if !strings.HasPrefix(tc.message, got.Message) {
				t.Error("message: got a value that is not a prefix of the original")
			}
		})
	}
}

func TestListCommitsErrors(t *testing.T) {
	failing := callbacks.HistoryCallbacks{
		ListCommits: func(context.Context, int, int) (callbacks.CommitListResult, error) {
			return callbacks.CommitListResult{}, errors.New("git log failed")
		},
	}
	unreached := callbacks.HistoryCallbacks{
		ListCommits: func(context.Context, int, int) (callbacks.CommitListResult, error) {
			t.Error("ListCommits callback invoked for a rejected call")
			return callbacks.CommitListResult{}, nil
		},
	}

	for _, tc := range []struct {
		name string
		cb   callbacks.HistoryCallbacks
		args map[string]any
		want map[string]any
	}{{
		name: "negative files_offset",
		cb:   unreached,
		args: map[string]any{"files_offset": float64(-1)},
		want: map[string]any{"error": "files_offset -1 must not be negative"},
	}, {
		name: "limit over maximum",
		cb:   unreached,
		args: map[string]any{"limit": float64(maxListCommitsLimit + 1)},
		want: map[string]any{"error": fmt.Sprintf("limit %d exceeds maximum of %d", maxListCommitsLimit+1, maxListCommitsLimit)},
	}, {
		name: "callback error",
		cb:   failing,
		args: map[string]any{"offset": float64(3), "limit": float64(5)},
		want: map[string]any{"error": "git log failed", "offset": 3, "limit": 5},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := callListCommits(t, tc.cb, tc.args)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("response (-want, +got):\n%s", diff)
			}
		})
	}
}
