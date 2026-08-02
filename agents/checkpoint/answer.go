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
	body := StripAnswerDelimiters(strings.TrimSpace(s))
	// Re-trim in case a stripped delimiter was all that padded the ends, then
	// cap. Truncation runs after stripping and can only shorten the body and
	// append the marker, so it can never reintroduce a delimiter.
	body = strings.TrimSpace(body)
	if body == "" {
		body = emptyAnswerPlaceholder
	} else {
		body = CapText(body, maxBytes)
	}
	return answerOpen + "\n" + body + "\n" + answerClose
}

// CapText returns s capped to maxBytes on a UTF-8 boundary, with a visible
// truncation marker appended when anything was cut (maxBytes <= 0 disables the
// cap). It is the exact cap [FrameAnswer] applies to the answer body, exported
// for the same reason [StripAnswerDelimiters] is: the strings a caller
// interpolates AROUND a framed answer are agent-authored, and the agent's side
// of a transcript is the side whose length nothing else bounds — a prompt that
// caps only the human's few sentences while interpolating the model's own
// question unbounded has capped the half that could not grow adversarially.
// Capping can only shorten the text and append the marker, so it can never
// reassemble a delimiter [StripAnswerDelimiters] removed.
func CapText(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return truncateUTF8(s, maxBytes) + truncationMarker
}

// StripAnswerDelimiters removes every occurrence of the answer frame's
// delimiters from s. It is the half of [FrameAnswer] that makes the frame a
// boundary, exported so that a caller assembling a prompt AROUND a framed
// answer can apply the same treatment to the surrounding text.
//
// That surrounding text needs it whenever it is not the prompt author's own
// prose. A transcript renderer that interpolates an agent-authored question
// beside a human's framed reply hands the agent the delimiters: text that
// closes the real frame and opens a fake one manufactures a human
// authorization the human never gave, in the one channel the prompt declares
// trustworthy. Stripping the delimiters out of every non-frame string is what
// keeps "inside the delimiters" equal to "written by a human".
//
// It is deliberately the SAME code the frame's own stripping runs, rather than
// a second implementation a caller would have to keep in step: a stripper that
// drifted from the delimiters [FrameAnswer] emits would leave exactly the gap
// it was written to close. Like that stripping it loops until no occurrence
// remains, so a nested payload cannot reassemble a delimiter out of the
// removed pieces. Nothing else about s is altered — no trimming, no capping —
// because the caller owns how its own text is presented.
func StripAnswerDelimiters(s string) string {
	for strings.Contains(s, answerOpen) || strings.Contains(s, answerClose) {
		s = strings.ReplaceAll(s, answerOpen, "")
		s = strings.ReplaceAll(s, answerClose, "")
	}
	return s
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
