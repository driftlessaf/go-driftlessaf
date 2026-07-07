/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// emptyJSONObject is the canonical empty JSON object used to replace
// empty or invalid tool-use Input fields.
var emptyJSONObject = json.RawMessage("{}")

// normalizeEmptyToolInputs walks the content blocks of a Message and replaces
// any tool_use block whose Input is nil, zero-length, or not valid JSON with
// the canonical empty JSON object "{}". Returns true if any block was changed.
//
// This repairs a mismatch between the Anthropic streaming API and Go's
// json.RawMessage: when a model emits a tool call with no input_json_delta
// events, the accumulated Input is a non-nil zero-length []byte that
// json.Marshal rejects ("unexpected end of JSON input"). Normalizing to "{}"
// makes the subsequent marshal succeed without altering real tool input.
func normalizeEmptyToolInputs(msg *anthropic.Message) bool {
	changed := false
	for i := range msg.Content {
		cb := &msg.Content[i]
		if cb.Type != "tool_use" {
			continue
		}
		if !isInputEmpty(cb.Input) {
			continue
		}
		cb.Input = emptyJSONObject
		changed = true
	}
	return changed
}

// normalizeEmptyTextBlocks removes text blocks whose Text is empty or
// whitespace-only from a Message's content. Returns true if any block was
// removed.
//
// During short provider-side anomaly windows the model can stream a
// degenerate text block with no text_delta events (a content_block_start
// carrying {"type":"text","text":""} immediately followed by
// content_block_stop). Message.ToParam copies the accumulated Text verbatim,
// and replaying the empty block on the next request makes the API reject it
// with a non-retryable 400 ("messages: text content blocks must be
// non-empty") that kills the conversation on what would otherwise be a
// successful turn. Stripping the degenerate blocks before the response is
// consumed keeps the replayed assistant message valid without altering real
// content (tool_use and thinking blocks are preserved). If stripping leaves
// the message with no content at all, callers must not send it.
func normalizeEmptyTextBlocks(msg *anthropic.Message) bool {
	before := len(msg.Content)
	msg.Content = slices.DeleteFunc(msg.Content, func(cb anthropic.ContentBlockUnion) bool {
		return cb.Type == "text" && strings.TrimSpace(cb.Text) == ""
	})
	return len(msg.Content) != before
}

// isInputEmpty reports whether a json.RawMessage is nil, zero-length, or
// not valid JSON -- all conditions that would cause json.Marshal to fail.
func isInputEmpty(input json.RawMessage) bool {
	return len(input) == 0 || !json.Valid(input)
}

// isEmptyRawMessageMarshalErr reports whether err is the specific
// json.Marshal failure triggered by a non-nil zero-length json.RawMessage.
// Only this error is safe to repair by normalizing the empty input; all other
// accumulate errors must propagate.
func isEmptyRawMessageMarshalErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unexpected end of JSON input")
}

// normalizeToolUseInput ensures a tool-use block's Input is valid JSON.
// If Input is nil, zero-length, or invalid, it is replaced with "{}".
// Used at tool-call consumption sites as defense in depth.
func normalizeToolUseInput(input json.RawMessage) json.RawMessage {
	if isInputEmpty(input) {
		return emptyJSONObject
	}
	return input
}
