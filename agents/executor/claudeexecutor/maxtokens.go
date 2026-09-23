/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"bytes"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

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

// TruncatedToolCallError reports that the output-token cap cut the model off
// while it was writing a call to Tool and the retries
// WithTruncatedToolCallRetries allows were spent. The cut-off call was never
// dispatched. Budget is the error's Unwrap, so it also matches
// *MaxTokensError.
type TruncatedToolCallError struct {
	Tool   string
	Budget *MaxTokensError
}

func (e *TruncatedToolCallError) Error() string {
	return fmt.Sprintf("Claude stopped at max_tokens while writing a %s tool call; the cut-off call was discarded (max_tokens=%d, output_tokens=%d)", e.Tool, e.Budget.MaxTokens, e.Budget.OutputTokens)
}

func (e *TruncatedToolCallError) Unwrap() error { return e.Budget }

// truncatedToolCall reports whether the cap cut message off mid tool call and
// returns that block's id and name. A max_tokens stop means generation ended
// early, so a trailing tool_use block is the call being written. The SDK
// replaces such a block's partial JSON with "{}" when the block closes; the
// validity check guards against an SDK that stops doing that. A complete,
// non-empty object never results from a cut. A zero-argument call closing
// exactly at the cap reads as cut too; no tool converted through
// claudetool.FromTool can produce one, since each requires a reasoning
// argument.
func truncatedToolCall(message anthropic.Message) (id, name string, ok bool) {
	if message.StopReason != anthropic.StopReasonMaxTokens || len(message.Content) == 0 {
		return "", "", false
	}
	last := message.Content[len(message.Content)-1]
	if last.Type != "tool_use" {
		return "", "", false
	}
	if !isInputEmpty(last.Input) && !bytes.Equal(last.Input, emptyJSONObject) {
		return "", "", false
	}
	return last.ID, last.Name, true
}

// truncatedToolCallResult is the error tool_result paired to a cut-off call
// in place of a handler's answer. Left to its own reading, the model takes a
// parse failure on the same call as its own mistake and rewrites the call at
// the same length, which cuts off again; this names the limit so the retry
// can shrink the input instead.
func truncatedToolCallResult(id, tool string, maxTokens, outputTokens int64) anthropic.ContentBlockParamUnion {
	text := fmt.Sprintf(
		"This %s call was cut off at the output-token limit (%d of %d tokens) and did not run. "+
			"Call %s again with a smaller input: keep the reasoning brief, leave out anything the tool does not need, and split the work across calls if the tool allows it.",
		tool, outputTokens, maxTokens, tool)
	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: id,
			IsError:   anthropic.Bool(true),
			Content: []anthropic.ToolResultBlockParamContentUnion{{
				OfText: &anthropic.TextBlockParam{Text: text},
			}},
		},
	}
}
