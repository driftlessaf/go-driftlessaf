/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/model"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// RefusalFallback is the model a turn refused by Anthropic's safety
// classifier is retried on. See WithRefusalFallback.
type RefusalFallback struct {
	// Messages is the Messages service that serves the fallback model, such
	// as an anthropic.Client's Messages. The zero value serves nothing and is
	// rejected.
	Messages anthropic.MessageService
	// ProviderModelID is the model ID sent to the provider.
	ProviderModelID string
	// LogicalModelID names the fallback's capability family, as
	// WithRoutedModel's logicalModelID does. Empty takes ProviderModelID.
	LogicalModelID string
	// Attribution is the fallback route's attribution, carried by its turns.
	// The zero value takes the executor's own, naming the fallback's logical
	// model wherever the executor's names one.
	Attribution agenttrace.Attribution
	// ResourceLabels are the fallback's billing labels, as WithResourceLabels
	// takes them. Nil takes the executor's own.
	ResourceLabels map[string]string
}

// servingModel is where one turn is sent: the configured model, or the
// refusal fallback.
type servingModel struct {
	messages       anthropic.MessageService
	modelName      string
	attribution    agenttrace.Attribution
	requestOptions []option.RequestOption
	telemetry      *telemetry.Recorder
}

// fallbackMismatch reports why the request e builds for its configured model
// would not suit fallback, the logical model of its refusal fallback, or ""
// when it would. Only a difference the request carries counts: the fallback
// may take more than the configured model, never less.
func (e *executor[Request, Response]) fallbackMismatch(fallback string) string {
	configured, alt := model.Resolve(e.capabilityModelName), model.Resolve(fallback)
	adaptive := e.summarizeThinking || (e.thinkingBudgetTokens != nil && !supportsExtendedThinkingBudget(e.capabilityModelName))
	switch {
	case supportsSamplingParams(e.capabilityModelName) && !supportsSamplingParams(fallback):
		return "it does not accept sampling parameters"
	case e.thinkingBudgetTokens != nil && supportsExtendedThinkingBudget(e.capabilityModelName) != supportsExtendedThinkingBudget(fallback):
		return "it takes a different thinking configuration"
	case adaptive && !alt.AdaptiveThinking:
		return "it does not accept adaptive thinking"
	case effortForModel(e.capabilityModelName, e.effort) != effortForModel(fallback, e.effort):
		return "it resolves the effort to a different level"
	case !e.omitToolReasoning && alt.OmitToolReasoning:
		// The tools ask for the reasoning parameter, which draws the very
		// reasoning_extraction refusals a fallback is there to recover from.
		return "it needs tool schemas without the reasoning parameter"
	case alt.AutomaticToolChoiceOnly && !configured.AutomaticToolChoiceOnly:
		return "it rejects a forced tool choice"
	case e.inputBudgetEnabled && alt.ContextWindow != 0 && alt.ContextWindow < e.contextWindow:
		return "its context window is smaller than the one the input budget measures against"
	}
	return ""
}

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
