/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"fmt"
	"slices"
	"strings"
)

// JoinStringsWithLimit joins non-blank, trimmed entries from blocks with
// separator, stopping before the combined length would exceed maxChars. If
// any blocks don't fit, moreNote is called with the count of blocks that
// were dropped entirely and the result is appended.
//
// When even the first block alone (after trimming) exceeds maxChars, it is
// shown truncated to maxChars rather than omitted, so the result is never
// empty just because one block is long; that block does not count toward
// the dropped total passed to moreNote, since it was shown (albeit cut
// short), not dropped.
//
// Blank entries (empty after trimming) are skipped entirely and never count
// toward the dropped total. Returns "" if blocks is empty or every entry is
// blank.
func JoinStringsWithLimit(blocks []string, maxChars int, separator string, moreNote func(dropped int) string) string {
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if t := strings.TrimSpace(b); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, line := range lines {
		// Length this line would add, including the separator joining it to
		// whatever has already been written.
		grow := len(line)
		if sb.Len() > 0 {
			grow += len(separator)
		}
		if sb.Len()+grow > maxChars {
			// dropped counts blocks with no content shown at all. Normally
			// that's every block from i onward, but if nothing had
			// accumulated yet, this block gets a partial write below and so
			// isn't fully dropped — don't double-count it.
			dropped := len(lines) - i
			if sb.Len() == 0 && maxChars > 0 {
				sb.WriteString(line[:min(len(line), maxChars)])
				dropped--
			}
			if dropped <= 0 {
				return sb.String()
			}
			return sb.String() + moreNote(dropped)
		}
		if sb.Len() > 0 {
			sb.WriteString(separator)
		}
		sb.WriteString(line)
	}
	return sb.String()
}

// SummarizeReasoning renders a concise, human-readable summary of a trace's
// extended-thinking blocks, suitable for display in a PR body or comment.
// It performs straightforward truncation rather than a second LLM call:
// blocks are rendered as a bulleted list and joined until adding the next
// one would exceed maxChars. If any blocks don't fit, a "+N more reasoning
// blocks" note replaces them.
//
// Returns "" if blocks is empty or contains only blank entries. Note that
// maxChars bounds the joined *bulleted* text (each non-blank block prefixed
// with "- "), so the rendered result can run a couple of characters over
// maxChars relative to the raw block content.
func SummarizeReasoning(blocks []ReasoningContent, maxChars int) string {
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if t := strings.TrimSpace(b.Thinking); t != "" {
			lines = append(lines, "- "+t)
		}
	}
	return JoinStringsWithLimit(lines, maxChars, "\n", moreReasoningBlocksNote)
}

// moreReasoningBlocksNote formats the "+N more reasoning block(s)" suffix
// SummarizeReasoning appends when truncation drops trailing blocks.
func moreReasoningBlocksNote(n int) string {
	if n == 1 {
		return fmt.Sprintf("\n+%d more reasoning block", n)
	}
	return fmt.Sprintf("\n+%d more reasoning blocks", n)
}

// DefaultRationaleToolNames lists the mutating tools whose per-call
// `reasoning` argument reads as a change rationale ("why I made this edit").
// Read-only exploration tools (read_file, search_codebase, ...) and
// result-submission tools are deliberately excluded: their rationales are
// procedural noise in a change summary.
var DefaultRationaleToolNames = []string{
	"edit_file", "write_file", "delete_file", "move_file", "copy_file",
	"chmod", "symlink",
}

// SummarizeTraceReasoning renders a concise, human-readable rationale for a
// completed trace, suitable for a PR body. It prefers the per-action
// reasoning recorded on mutating tool calls (see [DefaultRationaleToolNames]
// and [Trace.AttachToolCallReasoning]) — one bullet per distinct rationale,
// in call order — because those explain individual changes. When the trace
// carries no tool-call rationales it falls back to the extended-thinking
// blocks via [SummarizeReasoning]. Returns "" for a nil trace or when
// neither source has content.
func SummarizeTraceReasoning[T any](t *Trace[T], maxChars int) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	calls := make([]*ToolCall[T], len(t.ToolCalls))
	copy(calls, t.ToolCalls)
	reasoningBlocks := t.Reasoning
	t.mu.Unlock()

	lines := make([]string, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for _, tc := range calls {
		if !slices.Contains(DefaultRationaleToolNames, tc.Name) {
			continue
		}
		tc.mu.Lock()
		r, _ := tc.Params["reasoning"].(string)
		tc.mu.Unlock()
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		lines = append(lines, "- "+r)
	}
	if len(lines) > 0 {
		return JoinStringsWithLimit(lines, maxChars, "\n", moreRationalesNote)
	}
	return SummarizeReasoning(reasoningBlocks, maxChars)
}

// moreRationalesNote formats the "+N more" suffix SummarizeTraceReasoning
// appends when truncation drops trailing rationales.
func moreRationalesNote(n int) string {
	return fmt.Sprintf("\n+%d more", n)
}
