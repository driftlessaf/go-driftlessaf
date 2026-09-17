/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import "fmt"

// MaxTokensError reports that a turn stopped at the output-token cap
// (Message.StopReason == "max_tokens") without producing any content: no
// tool call and no text, typically because extended thinking consumed the
// whole budget. MaxTokens is the configured cap the request was sent with
// and OutputTokens is what the model actually emitted, so callers can decide
// between retrying and raising the budget without parsing the error string.
//
// The Error text keeps the "no content in Claude's response" prefix that the
// untyped fallback for other stop reasons produces. Nothing in the tree
// matches this type by substring: the prefix serves modelerr's untyped
// fallback rule, which classifies an already-wrapped error by message, and
// keeps log lines and failure signatures continuous, since operators search
// for that phrase.
type MaxTokensError struct {
	MaxTokens    int64
	OutputTokens int64
}

func (e *MaxTokensError) Error() string {
	return fmt.Sprintf("no content in Claude's response: stopped at max_tokens (max_tokens=%d, output_tokens=%d)", e.MaxTokens, e.OutputTokens)
}
