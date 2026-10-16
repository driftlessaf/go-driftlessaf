/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package callbacks_test

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
)

// A small page must not allocate the submitted file's full size, or every
// match it contains. Keep this serial so other tests do not affect TotalAlloc.
func TestLocalWorktree_PaginationBoundsAllocations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		setup func(*testing.T, *os.Root)
		call  func(*testing.T, callbacks.WorktreeCallbacks)
	}{
		{
			name:  "three bytes from a large file",
			files: map[string]string{"large.txt": "ok\n"},
			setup: func(t *testing.T, r *os.Root) {
				if err := os.Truncate(filepath.Join(r.Name(), "large.txt"), 32<<20); err != nil {
					t.Fatal(err)
				}
			},
			call: func(t *testing.T, cb callbacks.WorktreeCallbacks) {
				got, err := cb.ReadFile(t.Context(), "large.txt", 0, 3)
				if err != nil {
					t.Fatal(err)
				}
				want := callbacks.ReadResult{Content: "ok\n", NextOffset: new(int64(3)), Remaining: (32 << 20) - 3}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("ReadFile (-want, +got):\n%s", diff)
				}
			},
		},
		{
			name:  "one of many matches",
			files: map[string]string{"matches.txt": strings.Repeat("a", 128<<10)},
			call: func(t *testing.T, cb callbacks.WorktreeCallbacks) {
				got, err := cb.SearchCodebase(t.Context(), ".", "a", "", 0, 1)
				if err != nil {
					t.Fatal(err)
				}
				want := callbacks.SearchResult{Matches: []callbacks.Match{{Path: "matches.txt", Length: 1}}, NextOffset: new(1), HasMore: true}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("SearchCodebase (-want, +got):\n%s", diff)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := openTestRoot(t, tc.files)
			if tc.setup != nil {
				tc.setup(t, r)
			}
			cb := callbacks.LocalWorktree(r)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			tc.call(t, cb)
			runtime.ReadMemStats(&after)
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
				t.Errorf("page allocated %d bytes, want less than 8 MiB", allocated)
			}
		})
	}
}

func TestLocalWorktree_ReadFileCapsRequestedWindow(t *testing.T) {
	const size = 2 << 20
	r := openTestRoot(t, map[string]string{"file.txt": strings.Repeat("a", size)})
	cb := callbacks.LocalWorktree(r)
	for _, limit := range []int{-1, math.MaxInt} {
		got, err := cb.ReadFile(t.Context(), "file.txt", 0, limit)
		if err != nil {
			t.Fatal(err)
		}
		want := callbacks.ReadResult{Content: strings.Repeat("a", 1<<20), NextOffset: new(int64(1 << 20)), Remaining: 1 << 20}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("limit %d (-want, +got):\n%s", limit, diff)
		}
	}
}

func TestLocalWorktree_SearchLimits(t *testing.T) {
	r := openTestRoot(t, map[string]string{"matches.txt": strings.Repeat("a", 1002)})
	cb := callbacks.LocalWorktree(r)
	for _, limit := range []int{-1, 0, math.MaxInt} {
		got, err := cb.SearchCodebase(t.Context(), ".", "a", "", 0, limit)
		if err != nil {
			t.Fatal(err)
		}
		want := callbacks.SearchResult{Matches: make([]callbacks.Match, 1000), NextOffset: new(1000), HasMore: true}
		for i := range want.Matches {
			want.Matches[i] = callbacks.Match{Path: "matches.txt", Offset: int64(i), Length: 1}
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("limit %d (-want, +got):\n%s", limit, diff)
		}
	}
	got, err := cb.SearchCodebase(t.Context(), ".", "a", "", math.MaxInt, 1)
	if err == nil || !strings.Contains(err.Error(), "narrow the path or filter") {
		t.Errorf("oversized offset: got = %v, want a narrowing diagnostic", err)
	}
	if diff := cmp.Diff(callbacks.SearchResult{}, got); diff != "" {
		t.Errorf("rejected offset (-want, +got):\n%s", diff)
	}
}

func TestLocalWorktree_SearchOversizedFile(t *testing.T) {
	r := openTestRoot(t, map[string]string{"a.txt": "xxx", "z-large.txt": "x"})
	if err := os.Truncate(filepath.Join(r.Name(), "z-large.txt"), (4<<20)+1); err != nil {
		t.Fatal(err)
	}
	cb := callbacks.LocalWorktree(r)
	got, err := cb.SearchCodebase(t.Context(), ".", "x", "", 0, 50)
	if err == nil || !strings.Contains(err.Error(), "exceeds search byte limit") {
		t.Errorf("oversized file: got = %v, want byte-limit error", err)
	}
	if diff := cmp.Diff(callbacks.SearchResult{}, got); diff != "" {
		t.Errorf("failed search must not return partial matches (-want, +got):\n%s", diff)
	}
	// The continuation is known in a.txt, so the oversized later file must
	// not be opened to answer this page.
	got, err = cb.SearchCodebase(t.Context(), ".", "x", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := callbacks.SearchResult{Matches: []callbacks.Match{{Path: "a.txt", Length: 1}}, NextOffset: new(1), HasMore: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("first page (-want, +got):\n%s", diff)
	}
}

func TestLocalWorktree_SearchPages(t *testing.T) {
	r := openTestRoot(t, map[string]string{"a.txt": "ababa", "b.txt": "abaa"})
	cb := callbacks.LocalWorktree(r)
	for _, tc := range []struct {
		name          string
		pattern       string
		offset, limit int
		want          callbacks.SearchResult
	}{
		{
			name:    "page crosses files",
			pattern: "a", offset: 2, limit: 2,
			want: callbacks.SearchResult{
				Matches:    []callbacks.Match{{Path: "a.txt", Offset: 4, Length: 1}, {Path: "b.txt", Length: 1}},
				NextOffset: new(4), HasMore: true,
			},
		},
		{
			name:    "full final page has no continuation",
			pattern: "a", offset: 4, limit: 2,
			want: callbacks.SearchResult{Matches: []callbacks.Match{{Path: "b.txt", Offset: 2, Length: 1}, {Path: "b.txt", Offset: 3, Length: 1}}},
		},
		{
			name:    "zero-width anchors retain file positions",
			pattern: "^|$", offset: 1, limit: 2,
			want: callbacks.SearchResult{
				Matches:    []callbacks.Match{{Path: "a.txt", Offset: 5}, {Path: "b.txt"}},
				NextOffset: new(3), HasMore: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cb.SearchCodebase(t.Context(), ".", tc.pattern, "", tc.offset, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("page (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestLocalWorktree_SearchRejectsUnusableContinuation(t *testing.T) {
	r := openTestRoot(t, map[string]string{"file.txt": strings.Repeat("a", 10002)})
	got, err := callbacks.LocalWorktree(r).SearchCodebase(t.Context(), ".", "a", "", 10000, 1)
	if err == nil || !strings.Contains(err.Error(), "search continuation exceeds") {
		t.Errorf("continuation: got = %v, want an offset-limit error", err)
	}
	if diff := cmp.Diff(callbacks.SearchResult{}, got); diff != "" {
		t.Errorf("rejected continuation (-want, +got):\n%s", diff)
	}
}
