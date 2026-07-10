/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// tailBreakpointLimit bounds how many moving cache breakpoints the executor
// keeps on the conversation tail. Two are kept (the current tail and the
// previous one) rather than one: the API's cache lookup walks back at most ~20
// content blocks from each breakpoint, so when a single turn appends more
// blocks than that (a turn with many parallel tool calls), the new tail marker
// alone would miss the prior turn's cache entry and re-write the entire
// prefix. Retaining the previous tail marker pins a breakpoint exactly where
// the prior turn's cache entry ends, so only the newly appended blocks are
// written fresh.
const tailBreakpointLimit = 2

// tailPosition identifies one content block within params.Messages by index.
// Indices remain valid for the lifetime of an execution because the
// conversation loop only ever appends messages and never mutates the content
// slices of earlier messages.
type tailPosition struct {
	message int
	block   int
}

// tailBreakpoints tracks the moving cache_control markers placed on the
// conversation tail so the growing message history is cached incrementally.
//
// Without tail markers, only the static prefix (tool definitions, system
// prompt, and optionally the first user block) is cached: every turn re-sends
// the accumulated conversation — the rendered prompt plus every prior tool
// result — as full-price input tokens. With a marker on the last content
// block of the newest message, each turn's appended content is written to the
// cache once (1.25x input price) and read at 0.1x on every subsequent turn.
//
// tailBreakpoints is not safe for concurrent use; each execution owns one.
type tailBreakpoints struct {
	// limit bounds how many tail markers may be live at once. It is derived
	// at construction from the API budget left over by the static prefix, so
	// the request total never exceeds maxCacheBreakpoints even when a caller
	// placed its own markers on tool definitions. Zero disables tail caching
	// entirely — degrading to static-prefix-only caching — rather than letting
	// the API reject the whole request with a non-retryable 400.
	limit int
	// positions holds the currently marked blocks, oldest first, at most
	// limit entries.
	positions []tailPosition
}

// newTailBreakpoints builds a tracker whose marker budget is whatever remains
// of the API's maxCacheBreakpoints after the markers already present on the
// assembled request's static prefix, capped at tailBreakpointLimit.
func newTailBreakpoints(params anthropic.MessageNewParams) *tailBreakpoints {
	return &tailBreakpoints{
		limit: min(tailBreakpointLimit, maxCacheBreakpoints-staticBreakpointCount(params)),
	}
}

// staticBreakpointCount counts the cache_control markers present on the
// request's static prefix — the tool definitions and system blocks —
// including any a caller placed on its own tool definitions.
func staticBreakpointCount(params anthropic.MessageNewParams) int {
	n := 0
	for i := range params.Tools {
		if t := params.Tools[i].OfTool; t != nil && hasBreakpoint(t.CacheControl) {
			n++
		}
	}
	for i := range params.System {
		if hasBreakpoint(params.System[i].CacheControl) {
			n++
		}
	}
	return n
}

// hasBreakpoint reports whether a cache_control marker is set. The SDK tags
// the field omitzero, so the zero value marshals as no marker.
func hasBreakpoint(cc anthropic.CacheControlEphemeralParam) bool {
	return !param.IsOmitted(cc)
}

// advance moves the tail breakpoint to the last cacheable content block of the
// newest message, clearing the oldest tracked marker once the limit is
// exceeded. It is a no-op when the tail is already marked (no new messages
// were appended), when the newest message has no block that supports
// cache_control, or when the static prefix left no marker budget.
func (tb *tailBreakpoints) advance(messages []anthropic.MessageParam) {
	if tb.limit <= 0 {
		return
	}
	pos, ok := lastCacheableBlock(messages)
	if !ok {
		return
	}
	if n := len(tb.positions); n > 0 && tb.positions[n-1] == pos {
		return
	}
	*blockCacheControl(&messages[pos.message].Content[pos.block]) = anthropic.NewCacheControlEphemeralParam()
	tb.positions = append(tb.positions, pos)
	for len(tb.positions) > tb.limit {
		old := tb.positions[0]
		// Clearing assigns the zero value; the SDK tags CacheControl omitzero,
		// so the marker disappears from the marshaled request.
		*blockCacheControl(&messages[old.message].Content[old.block]) = anthropic.CacheControlEphemeralParam{}
		tb.positions = tb.positions[1:]
	}
}

// lastCacheableBlock returns the position of the last content block of the
// last message that supports a cache_control marker, walking backward within
// that message past block types that cannot carry one.
func lastCacheableBlock(messages []anthropic.MessageParam) (tailPosition, bool) {
	if len(messages) == 0 {
		return tailPosition{}, false
	}
	m := len(messages) - 1
	for b := len(messages[m].Content) - 1; b >= 0; b-- {
		if blockCacheControl(&messages[m].Content[b]) != nil {
			return tailPosition{message: m, block: b}, true
		}
	}
	return tailPosition{}, false
}

// blockCacheControl returns a pointer to the block variant's CacheControl
// field, or nil for variants that cannot carry a cache_control marker (the
// thinking blocks). Support checks and marker writes both go through this one
// switch, so the two can never diverge. The conversation loop only appends
// text, tool_use, and tool_result blocks; the remaining variants are covered
// so a future block type degrades to "skip" rather than a panic.
func blockCacheControl(block *anthropic.ContentBlockParamUnion) *anthropic.CacheControlEphemeralParam {
	switch {
	case block.OfText != nil:
		return &block.OfText.CacheControl
	case block.OfToolResult != nil:
		return &block.OfToolResult.CacheControl
	case block.OfToolUse != nil:
		return &block.OfToolUse.CacheControl
	case block.OfImage != nil:
		return &block.OfImage.CacheControl
	case block.OfDocument != nil:
		return &block.OfDocument.CacheControl
	case block.OfSearchResult != nil:
		return &block.OfSearchResult.CacheControl
	case block.OfServerToolUse != nil:
		return &block.OfServerToolUse.CacheControl
	case block.OfWebSearchToolResult != nil:
		return &block.OfWebSearchToolResult.CacheControl
	case block.OfWebFetchToolResult != nil:
		return &block.OfWebFetchToolResult.CacheControl
	case block.OfCodeExecutionToolResult != nil:
		return &block.OfCodeExecutionToolResult.CacheControl
	case block.OfBashCodeExecutionToolResult != nil:
		return &block.OfBashCodeExecutionToolResult.CacheControl
	case block.OfTextEditorCodeExecutionToolResult != nil:
		return &block.OfTextEditorCodeExecutionToolResult.CacheControl
	case block.OfToolSearchToolResult != nil:
		return &block.OfToolSearchToolResult.CacheControl
	case block.OfContainerUpload != nil:
		return &block.OfContainerUpload.CacheControl
	case block.OfMidConvSystem != nil:
		return &block.OfMidConvSystem.CacheControl
	}
	return nil
}

// seedFirstUserTail returns the initial tail positions for a tracker: when
// the WithCacheFirstUserBlock option placed a marker on the first user block
// during assembly, that marker is adopted as the initial tail so the rotation
// accounts for it against the budget and eventually reclaims its slot. Empty
// when the option is off or the marker was not actually placed (for example,
// suppressed because caller-supplied markers exhausted the budget).
func seedFirstUserTail(cacheControl, cacheFirstUserBlock bool, params anthropic.MessageNewParams) []tailPosition {
	if !cacheControl || !cacheFirstUserBlock {
		return nil
	}
	if len(params.Messages) == 0 || len(params.Messages[0].Content) == 0 {
		return nil
	}
	if tb := params.Messages[0].Content[0].OfText; tb == nil || !hasBreakpoint(tb.CacheControl) {
		return nil
	}
	return []tailPosition{{message: 0, block: 0}}
}
