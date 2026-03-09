/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

import (
	"strings"
	"testing"
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
