/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
)

func TestFindingReadContent(t *testing.T) {
	t.Run("reads all content from offset 0", func(t *testing.T) {
		content, next, remaining := findingReadContent("hello world", 0, defaultFindingReadLimit)
		if content != "hello world" {
			t.Errorf("content: got = %q, want %q", content, "hello world")
		}
		if next != nil {
			t.Errorf("next_offset: got = %d, want nil", *next)
		}
		if remaining != 0 {
			t.Errorf("remaining: got = %d, want 0", remaining)
		}
	})

	t.Run("reads with offset and limit", func(t *testing.T) {
		content, next, remaining := findingReadContent("abcdefghij", 3, 4)
		if content != "defg" {
			t.Errorf("content: got = %q, want %q", content, "defg")
		}
		if next == nil {
			t.Fatal("next_offset: got nil, want non-nil")
		}
		if *next != 7 {
			t.Errorf("next_offset: got = %d, want 7", *next)
		}
		if remaining != 3 {
			t.Errorf("remaining: got = %d, want 3", remaining)
		}
	})

	t.Run("reads empty content", func(t *testing.T) {
		content, next, remaining := findingReadContent("", 0, defaultFindingReadLimit)
		if content != "" {
			t.Errorf("content: got = %q, want empty", content)
		}
		if next != nil {
			t.Errorf("next_offset: got = %d, want nil", *next)
		}
		if remaining != 0 {
			t.Errorf("remaining: got = %d, want 0", remaining)
		}
	})

	t.Run("offset beyond end returns empty", func(t *testing.T) {
		content, next, _ := findingReadContent("short", 1000, 100)
		if content != "" {
			t.Errorf("content: got = %q, want empty", content)
		}
		if next != nil {
			t.Errorf("next_offset: got = %d, want nil", *next)
		}
	})

	t.Run("reads to exact EOF", func(t *testing.T) {
		content, next, remaining := findingReadContent("exact", 0, 5)
		if content != "exact" {
			t.Errorf("content: got = %q, want %q", content, "exact")
		}
		if next != nil {
			t.Errorf("next_offset: got = %d, want nil at EOF", *next)
		}
		if remaining != 0 {
			t.Errorf("remaining: got = %d, want 0 at EOF", remaining)
		}
	})

	t.Run("negative offset clamped to 0", func(t *testing.T) {
		content, _, _ := findingReadContent("hello", -5, 3)
		if content != "hel" {
			t.Errorf("content: got = %q, want %q", content, "hel")
		}
	})

	t.Run("limit clamped to maxFindingReadLimit", func(t *testing.T) {
		// Build content larger than maxFindingReadLimit to verify clamping.
		s := strings.Repeat("x", maxFindingReadLimit+100)
		content, next, _ := findingReadContent(s, 0, maxFindingReadLimit+999)
		if len(content) != maxFindingReadLimit {
			t.Errorf("content length: got = %d, want %d", len(content), maxFindingReadLimit)
		}
		if next == nil {
			t.Error("next_offset: got nil, want non-nil when clamped limit leaves remaining bytes")
		}
	})
}

// assertCompactPointers verifies every match contains exactly "offset" and "length",
// with no "line" or other fields — enforcing the compact match pointer contract.
func assertCompactPointers(t *testing.T, matches []map[string]any) {
	t.Helper()
	for i, m := range matches {
		if _, ok := m["offset"]; !ok {
			t.Errorf("match[%d]: missing 'offset' field", i)
		}
		if _, ok := m["length"]; !ok {
			t.Errorf("match[%d]: missing 'length' field", i)
		}
		if _, hasLine := m["line"]; hasLine {
			t.Errorf("match[%d]: unexpected 'line' field — search returns compact pointers only", i)
		}
		if len(m) != 2 {
			t.Errorf("match[%d]: got = %d fields, want exactly 2 (offset, length); extra keys: %v", i, len(m), m)
		}
	}
}

func TestFindingSearchContent(t *testing.T) {
	t.Run("basic regex match", func(t *testing.T) {
		s := "line1: ERROR something\nline2: ok\nline3: ERROR again\n"
		matches, total, err := findingSearchContent(s, "ERROR", 0, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 2 {
			t.Fatalf("total: got = %d, want 2", total)
		}
		if len(matches) != 2 {
			t.Fatalf("len(matches): got = %d, want 2", len(matches))
		}
		assertCompactPointers(t, matches)
	})

	t.Run("no matches returns empty slice", func(t *testing.T) {
		matches, total, err := findingSearchContent("all good\nno issues\n", "ERROR", 0, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 0 {
			t.Errorf("total: got = %d, want 0", total)
		}
		if len(matches) != 0 {
			t.Errorf("len(matches): got = %d, want 0", len(matches))
		}
	})

	t.Run("pagination with skip", func(t *testing.T) {
		matches, total, err := findingSearchContent("err1\nerr2\nerr3\nerr4\nerr5\n", "err", 2, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 5 {
			t.Fatalf("total: got = %d, want 5", total)
		}
		if len(matches) != 2 {
			t.Fatalf("len(matches): got = %d, want 2", len(matches))
		}
		assertCompactPointers(t, matches)
	})

	t.Run("match offset is byte position", func(t *testing.T) {
		// aaa=0-2, \n=3, bbb=4-6, \n=7
		matches, _, err := findingSearchContent("aaa\nbbb\nccc\n", "bbb", 0, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("len(matches): got = %d, want 1", len(matches))
		}
		assertCompactPointers(t, matches)
		if matches[0]["offset"] != int64(4) {
			t.Errorf("offset: got = %v, want 4", matches[0]["offset"])
		}
		if matches[0]["length"] != 3 {
			t.Errorf("length: got = %v, want 3", matches[0]["length"])
		}
	})

	t.Run("invalid regex returns error", func(t *testing.T) {
		_, _, err := findingSearchContent("some content", "[invalid", 0, 20)
		if err == nil {
			t.Fatal("expected error for invalid regex, got nil")
		}
	})

	t.Run("pattern too long returns error", func(t *testing.T) {
		long := strings.Repeat("a", maxFindingPatternLength+1)
		_, _, err := findingSearchContent("some content", long, 0, 20)
		if err == nil {
			t.Fatal("expected error for pattern exceeding max length, got nil")
		}
	})

	t.Run("match count is capped at maxFindingSearchMatches", func(t *testing.T) {
		var sb strings.Builder
		for range maxFindingSearchMatches + 500 {
			sb.WriteString("err\n")
		}
		_, total, err := findingSearchContent(sb.String(), "err", 0, maxFindingSearchMatches+500)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total > maxFindingSearchMatches {
			t.Errorf("total: got = %d, want ≤ %d", total, maxFindingSearchMatches)
		}
	})
}

// TestFindingToolsConcurrentDuplicateCalls is a race regression test: executors
// dispatch a turn's tool calls concurrently, so the duplicate-call guards and the
// shared log cache must be safe for concurrent handler invocations. Run with -race.
func TestFindingToolsConcurrentDuplicateCalls(t *testing.T) {
	const workers = 10

	// run invokes the handler with the same call from several goroutines at once
	// and returns how many invocations were short-circuited as duplicates.
	run := func(t *testing.T, tool Tool[string], call ToolCall) (dups int) {
		t.Helper()
		trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
		results := make([]map[string]any, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				results[i] = tool.Handler(t.Context(), call, trace, nil)
			})
		}
		wg.Wait()
		for _, resp := range results {
			if _, ok := resp["error"]; ok {
				dups++
			}
		}
		return dups
	}

	t.Run("retry_finding dedup guard", func(t *testing.T) {
		var retries atomic.Int64
		tool := retryFindingTool[string](func(context.Context, callbacks.FindingKind, string) error {
			retries.Add(1)
			return nil
		})
		dups := run(t, tool, ToolCall{
			ID:   "retry-1",
			Name: "retry_finding",
			Args: map[string]any{"kind": "check", "identifier": "flaky-ci"},
		})
		if got := retries.Load(); got != 1 {
			t.Errorf("retry callback invocations: got = %d, want 1", got)
		}
		if dups != workers-1 {
			t.Errorf("duplicate responses: got = %d, want %d", dups, workers-1)
		}
	})

	t.Run("read_finding_logs dedup guard", func(t *testing.T) {
		var fetches atomic.Int64
		tools := findingLogTools[string](func(context.Context, callbacks.FindingKind, string) (string, error) {
			fetches.Add(1)
			return "log line one\nlog line two\n", nil
		})
		dups := run(t, tools["read_finding_logs"], ToolCall{
			ID:   "read-1",
			Name: "read_finding_logs",
			Args: map[string]any{"kind": "check", "identifier": "flaky-ci"},
		})
		if got := fetches.Load(); got != 1 {
			t.Errorf("getLogs invocations: got = %d, want 1", got)
		}
		if dups != workers-1 {
			t.Errorf("duplicate responses: got = %d, want %d", dups, workers-1)
		}
	})

	t.Run("search_finding_logs dedup guard", func(t *testing.T) {
		var fetches atomic.Int64
		tools := findingLogTools[string](func(context.Context, callbacks.FindingKind, string) (string, error) {
			fetches.Add(1)
			return "log line one\nlog line two\n", nil
		})
		dups := run(t, tools["search_finding_logs"], ToolCall{
			ID:   "search-1",
			Name: "search_finding_logs",
			Args: map[string]any{"kind": "check", "identifier": "flaky-ci", "pattern": "line"},
		})
		if got := fetches.Load(); got != 1 {
			t.Errorf("getLogs invocations: got = %d, want 1", got)
		}
		if dups != workers-1 {
			t.Errorf("duplicate responses: got = %d, want %d", dups, workers-1)
		}
	})

	t.Run("log cache shared across read and search", func(t *testing.T) {
		tools := findingLogTools[string](func(context.Context, callbacks.FindingKind, string) (string, error) {
			return "log line one\nlog line two\n", nil
		})
		trace, _ := agenttrace.StartTrace[string](t.Context(), "test")
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				// Vary offset/skip so the dedup guards do not short-circuit:
				// every invocation exercises the shared log cache.
				if i%2 == 0 {
					tools["read_finding_logs"].Handler(t.Context(), ToolCall{
						ID:   "read-cache",
						Name: "read_finding_logs",
						Args: map[string]any{"kind": "check", "identifier": "flaky-ci", "offset": float64(i)},
					}, trace, nil)
				} else {
					tools["search_finding_logs"].Handler(t.Context(), ToolCall{
						ID:   "search-cache",
						Name: "search_finding_logs",
						Args: map[string]any{"kind": "check", "identifier": "flaky-ci", "pattern": "line", "skip": float64(i)},
					}, trace, nil)
				}
			})
		}
		wg.Wait()
	})
}
