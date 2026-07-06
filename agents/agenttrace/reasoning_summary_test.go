/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"fmt"
	"strings"
	"testing"
)

func numberedMoreNote(n int) string {
	return fmt.Sprintf("|+%d", n)
}

func TestJoinStringsWithLimit_Empty(t *testing.T) {
	if got := JoinStringsWithLimit(nil, 400, "\n", numberedMoreNote); got != "" {
		t.Errorf("JoinStringsWithLimit(nil) = %q, want empty", got)
	}
	if got := JoinStringsWithLimit([]string{}, 400, "\n", numberedMoreNote); got != "" {
		t.Errorf("JoinStringsWithLimit(empty slice) = %q, want empty", got)
	}
}

func TestJoinStringsWithLimit_BlankEntriesOnly(t *testing.T) {
	entries := []string{"   ", "\n\t", ""}
	if got := JoinStringsWithLimit(entries, 400, "\n", numberedMoreNote); got != "" {
		t.Errorf("JoinStringsWithLimit(blank entries) = %q, want empty", got)
	}
}

func TestJoinStringsWithLimit_SingleShortEntry(t *testing.T) {
	entries := []string{"hello world"}
	want := "hello world"
	if got := JoinStringsWithLimit(entries, 400, "\n", numberedMoreNote); got != want {
		t.Errorf("JoinStringsWithLimit() = %q, want %q", got, want)
	}
}

func TestJoinStringsWithLimit_TrimsWhitespace(t *testing.T) {
	entries := []string{"  \n  padded  \n  "}
	want := "padded"
	if got := JoinStringsWithLimit(entries, 400, "\n", numberedMoreNote); got != want {
		t.Errorf("JoinStringsWithLimit() = %q, want %q", got, want)
	}
}

func TestJoinStringsWithLimit_MultipleEntriesUnderCap(t *testing.T) {
	entries := []string{"first", "second", "third"}
	want := "first|second|third"
	got := JoinStringsWithLimit(entries, 400, "|", numberedMoreNote)
	if got != want {
		t.Errorf("JoinStringsWithLimit() = %q, want %q", got, want)
	}
}

func TestJoinStringsWithLimit_ExceedsCapDropsTrailingEntries(t *testing.T) {
	entries := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"} // 10 chars each
	// Separator "|" (1 char): first entry fits (10), second would need 21.
	got := JoinStringsWithLimit(entries, 10, "|", numberedMoreNote)

	if !strings.HasPrefix(got, "aaaaaaaaaa") {
		t.Fatalf("JoinStringsWithLimit() = %q, want prefix %q", got, "aaaaaaaaaa")
	}
	if !strings.HasSuffix(got, "|+2") {
		t.Errorf("JoinStringsWithLimit() = %q, want suffix %q", got, "|+2")
	}
	if strings.Contains(got, "bbbbbbbbbb") || strings.Contains(got, "cccccccccc") {
		t.Errorf("JoinStringsWithLimit() = %q, want dropped entries omitted", got)
	}
}

func TestJoinStringsWithLimit_FirstEntryExceedsCapAlone(t *testing.T) {
	// The only entry exceeds the cap: it is shown truncated, and since
	// nothing else exists, moreNote must not be called at all (dropped <= 0).
	entries := []string{strings.Repeat("x", 1000)}
	got := JoinStringsWithLimit(entries, 50, "\n", numberedMoreNote)

	if got == "" {
		t.Fatal("JoinStringsWithLimit() = empty, want the truncated entry")
	}
	if len(got) != 50 {
		t.Errorf("JoinStringsWithLimit() length = %d, want exactly maxChars (50)", len(got))
	}
	if strings.Contains(got, "|+") {
		t.Errorf("JoinStringsWithLimit() = %q, want no moreNote call since nothing was dropped", got)
	}
}

func TestJoinStringsWithLimit_FirstEntryExceedsCapWithMoreAfter(t *testing.T) {
	// The first entry alone exceeds the cap and is shown truncated; a second
	// entry exists and is genuinely dropped. dropped must count only the
	// truly omitted entry, not the partially-shown first one.
	entries := []string{strings.Repeat("x", 1000), "dropped entirely"}
	got := JoinStringsWithLimit(entries, 50, "\n", numberedMoreNote)

	if !strings.HasSuffix(got, "|+1") {
		t.Errorf("JoinStringsWithLimit() = %q, want suffix %q", got, "|+1")
	}
	if strings.Contains(got, "dropped entirely") {
		t.Errorf("JoinStringsWithLimit() = %q, want the second entry's content omitted", got)
	}
}

func TestSummarizeReasoning_Empty(t *testing.T) {
	if got := SummarizeReasoning(nil, 400); got != "" {
		t.Errorf("SummarizeReasoning(nil) = %q, want empty", got)
	}
	if got := SummarizeReasoning([]ReasoningContent{}, 400); got != "" {
		t.Errorf("SummarizeReasoning(empty slice) = %q, want empty", got)
	}
}

func TestSummarizeReasoning_BlankBlocksOnly(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "   "},
		{Thinking: "\n\t"},
		{Thinking: ""},
	}
	if got := SummarizeReasoning(blocks, 400); got != "" {
		t.Errorf("SummarizeReasoning(blank blocks) = %q, want empty", got)
	}
}

func TestSummarizeReasoning_SingleShortBlock(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "Considered two approaches and picked the simpler one."},
	}
	want := "- Considered two approaches and picked the simpler one."
	if got := SummarizeReasoning(blocks, 400); got != want {
		t.Errorf("SummarizeReasoning() = %q, want %q", got, want)
	}
}

func TestSummarizeReasoning_MultipleBlocksUnderCap(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "First, explored the codebase structure."},
		{Thinking: "Then, identified the relevant package."},
		{Thinking: "Finally, implemented the fix."},
	}
	want := "- First, explored the codebase structure.\n" +
		"- Then, identified the relevant package.\n" +
		"- Finally, implemented the fix."
	got := SummarizeReasoning(blocks, 400)
	if got != want {
		t.Errorf("SummarizeReasoning() = %q, want %q", got, want)
	}
	if strings.Contains(got, "more reasoning block") {
		t.Errorf("SummarizeReasoning() unexpectedly truncated: %q", got)
	}
}

func TestSummarizeReasoning_ExceedsCapTruncatesWithNote(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "aaaaaaaaaa"}, // 10 chars -> "- aaaaaaaaaa" = 12 chars
		{Thinking: "bbbbbbbbbb"},
		{Thinking: "cccccccccc"},
	}
	// Cap fits exactly the first entry (12 chars) but not a second (12 + 1 + 12 = 25).
	got := SummarizeReasoning(blocks, 12)

	if !strings.HasPrefix(got, "- aaaaaaaaaa") {
		t.Fatalf("SummarizeReasoning() = %q, want prefix %q", got, "- aaaaaaaaaa")
	}
	if !strings.Contains(got, "+2 more reasoning blocks") {
		t.Errorf("SummarizeReasoning() = %q, want a %q note", got, "+2 more reasoning blocks")
	}
	if strings.Contains(got, "bbbbbbbbbb") || strings.Contains(got, "cccccccccc") {
		t.Errorf("SummarizeReasoning() = %q, want remaining blocks omitted", got)
	}
}

func TestSummarizeReasoning_ExceedsCapSingularNote(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "aaaaaaaaaa"},
		{Thinking: "bbbbbbbbbb"},
	}
	got := SummarizeReasoning(blocks, 12)

	if !strings.Contains(got, "+1 more reasoning block") {
		t.Errorf("SummarizeReasoning() = %q, want a %q note", got, "+1 more reasoning block")
	}
	if strings.Contains(got, "+1 more reasoning blocks") {
		t.Errorf("SummarizeReasoning() = %q, want singular phrasing, not plural", got)
	}
}

func TestSummarizeReasoning_FirstBlockExceedsCapAlone(t *testing.T) {
	// A single block longer than maxChars on its own still returns something
	// bounded and doesn't panic. Since this is the only block and it is
	// shown (truncated), nothing was actually dropped, so no "+N more" note
	// should appear.
	blocks := []ReasoningContent{
		{Thinking: strings.Repeat("x", 1000)},
	}
	got := SummarizeReasoning(blocks, 50)
	if got == "" {
		t.Fatal("SummarizeReasoning() = empty, want non-empty even when the only block exceeds the cap")
	}
	if strings.Contains(got, "more reasoning block") {
		t.Errorf("SummarizeReasoning() = %q, want no \"more reasoning block\" note since the only block was shown (truncated), not dropped", got)
	}
}

func TestSummarizeReasoning_SkipsBlankBlocksAmongContent(t *testing.T) {
	blocks := []ReasoningContent{
		{Thinking: "first"},
		{Thinking: "   "},
		{Thinking: "second"},
	}
	want := "- first\n- second"
	if got := SummarizeReasoning(blocks, 400); got != want {
		t.Errorf("SummarizeReasoning() = %q, want %q", got, want)
	}
}
