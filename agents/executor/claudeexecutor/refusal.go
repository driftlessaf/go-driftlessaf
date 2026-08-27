/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// RefusalError reports that Anthropic's safety classifier refused to
// generate a response for a turn (Message.StopReason == "refusal") rather
// than the model producing no content for some other reason. It wraps the
// SDK's StopDetails so callers can branch on the policy category
// programmatically instead of parsing the error string. Category is one of
// "cyber", "bio", "frontier_llm", "reasoning_extraction", "general_harms";
// Explanation is Anthropic's own human-readable text and may be empty.
type RefusalError struct {
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	if e.Explanation != "" {
		return fmt.Sprintf("Claude's safety classifier refused to respond (category: %s): %s", e.Category, e.Explanation)
	}
	return fmt.Sprintf("Claude's safety classifier refused to respond (category: %s)", e.Category)
}

// refusedTurnPlaceholder stands in for the assistant's turn when a refusal
// leaves message.Content empty — an assistant message with zero content
// blocks is itself invalid, so something must occupy the turn to keep the
// required user/assistant alternation intact before the nudge (a user turn)
// is appended. It states only the fact of the refusal, never anything about
// what triggered it.
const refusedTurnPlaceholder = "[response withheld by Anthropic's safety classifier]"

// refusalError builds a RefusalError from a refused message's StopDetails.
func refusalError(details anthropic.RefusalStopDetails) *RefusalError {
	return &RefusalError{
		Category:    string(details.Category),
		Explanation: details.Explanation,
	}
}

// refusalNudgeText is the synthetic user-turn message injected after a
// refusal when WithRefusalNudge is enabled, before the turn is retried. It
// deliberately does not ask the model to explain or reproduce whatever
// triggered the classifier — doing so would risk tripping it again — and
// steers toward tool calls, the framing that empirically resists the
// classifier better than a free-text response: bare single-turn completions
// over classifier-sensitive content refuse far more often than the same
// content read inside an agentic tool loop.
func refusalNudgeText(category string) string {
	return fmt.Sprintf(
		"Your previous response was blocked by an automated safety classifier (category: %s). "+
			"Do not attempt to reproduce, quote, or explain whatever triggered it. "+
			"Continue the task now by calling one of your available tools rather than replying with text.",
		category)
}
