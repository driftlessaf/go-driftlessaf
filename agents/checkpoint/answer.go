/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"strings"
	"unicode/utf8"
)

const (
	// answerOpen/answerClose delimit the human answer so the model cannot
	// confuse free-form human text for its own prior instructions. They are
	// intentionally distinctive and unlikely to occur in normal prose.
	answerOpen  = "<<<BEGIN HUMAN ANSWER>>>"
	answerClose = "<<<END HUMAN ANSWER>>>"

	// emptyAnswerPlaceholder is substituted when the human answer is empty (or
	// only whitespace). An empty tool_result string is what caused the resume
	// 400s the suspend/resume design set out to prevent, so an answer is never
	// allowed to be empty on the wire.
	emptyAnswerPlaceholder = "(the human did not provide an answer)"

	// truncationMarker is appended when the answer is capped.
	truncationMarker = "…[truncated]"
)

// FrameAnswer prepares a raw human answer for injection as a tool_result: it
// strips any embedded delimiter strings, substitutes a placeholder for an
// empty answer, caps the body to maxBytes on a UTF-8 boundary (maxBytes <= 0
// disables the cap), and wraps the result in distinctive delimiters. Framing
// is what keeps a paused agent from treating arbitrary human input as trusted
// instructions, and the empty-substitution is what keeps the resumed provider
// request from carrying an empty tool_result.
//
// Delimiter stripping is what makes the frame a boundary: without it, an
// answer containing the literal closing delimiter would end the frame early
// and smuggle the rest of the answer outside it, where the model reads it as
// top-level instructions. Stripping loops until no occurrence remains, so a
// nested payload cannot reassemble a delimiter out of the removed pieces.
func FrameAnswer(s string, maxBytes int) string {
	body := strings.TrimSpace(s)
	for strings.Contains(body, answerOpen) || strings.Contains(body, answerClose) {
		body = strings.ReplaceAll(body, answerOpen, "")
		body = strings.ReplaceAll(body, answerClose, "")
	}
	// Re-trim in case a stripped delimiter was all that padded the ends, then
	// cap. Truncation runs after stripping and can only shorten the body and
	// append the marker, so it can never reintroduce a delimiter.
	body = strings.TrimSpace(body)
	if body == "" {
		body = emptyAnswerPlaceholder
	} else if maxBytes > 0 && len(body) > maxBytes {
		body = truncateUTF8(body, maxBytes) + truncationMarker
	}
	return answerOpen + "\n" + body + "\n" + answerClose
}

// truncateUTF8 returns the longest prefix of s that is at most maxBytes bytes
// and does not split a multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Back off to the start of the rune that straddles the byte limit.
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
