/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/retry"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
)

// countFramingTokens covers the tokens count_tokens adds for message and tool
// framing on top of the request's own bytes.
const countFramingTokens = 4096

// InputTooLargeError reports that a turn's request did not fit the input
// budget even after older tool results were reduced, so the provider was not
// called. Budget is ContextWindow minus OutputHeadroom (the request's
// max_tokens) minus a 1% tolerance. InputTokens is the first exact count and
// ReducedInputTokens the last one; they are equal when nothing was reduced.
//
// The Error text deliberately avoids the phrases that classify an error as a
// stream failure, a turn-limit stop or a transient API error, so string-based
// classifiers do not mistake it for any of them. Match it with errors.AsType,
// or with errors.Is against executor.ErrInputTooLarge.
type InputTooLargeError struct {
	Model              string
	ContextWindow      int64
	OutputHeadroom     int64
	Budget             int64
	InputTokens        int64
	ReducedInputTokens int64
	ReducedResults     int
}

var _ error = (*InputTooLargeError)(nil)

func (e *InputTooLargeError) Error() string {
	return fmt.Sprintf("Claude request does not fit the input budget of model %q: %d input tokens, %d after reducing %d older tool results, budget %d (context window %d, output headroom %d)",
		e.Model, e.InputTokens, e.ReducedInputTokens, e.ReducedResults, e.Budget, e.ContextWindow, e.OutputHeadroom)
}

func (e *InputTooLargeError) Unwrap() error { return agentexecutor.ErrInputTooLarge }

// inputBudget is the input token allowance for a request: the window less the
// output the request may generate, less 1% for drift between count_tokens and
// the serving count.
func inputBudget(window, maxTokens int64) int64 {
	return window - maxTokens - window/100
}

// countParams builds the count_tokens request for params. It carries only the
// fields count_tokens accepts on every provider; output_config is left out
// because it does not change the input count.
func countParams(params anthropic.MessageNewParams) (anthropic.MessageCountTokensParams, error) {
	cp := anthropic.MessageCountTokensParams{
		Model:      params.Model,
		Messages:   params.Messages,
		Thinking:   params.Thinking,
		ToolChoice: params.ToolChoice,
	}
	if len(params.System) > 0 {
		cp.System.OfTextBlockArray = params.System
	}
	if len(params.Tools) > 0 {
		cp.Tools = make([]anthropic.MessageCountTokensToolUnionParam, 0, len(params.Tools))
	}
	for i, t := range params.Tools {
		// Dropping a tool would undercount the request, so an unconvertible
		// tool fails the count rather than letting an oversized request through.
		if t.OfTool == nil {
			return cp, fmt.Errorf("tool %d is not a custom tool and cannot be counted", i)
		}
		cp.Tools = append(cp.Tools, anthropic.MessageCountTokensToolUnionParam{OfTool: t.OfTool})
	}
	return cp, nil
}

// upperBoundTokens bounds the token count of cp without a network call. A text
// token is never shorter than one byte of the encoded request, so a request
// whose bound fits the budget fits without an exact count. Image and document
// blocks are tokenised from the decoded media, not their JSON size, so a
// request that carries one has no local bound.
func upperBoundTokens(cp anthropic.MessageCountTokensParams) int64 {
	if hasMedia(cp.Messages) {
		return math.MaxInt64
	}
	body, err := json.Marshal(cp)
	if err != nil {
		// No bound can be proven, so fall through to the exact count.
		return math.MaxInt64
	}
	return int64(len(body)) + countFramingTokens
}

// hasMedia reports whether any message, or any tool_result inside one,
// carries an image or document block. The system prompt is text blocks only,
// so it cannot carry media.
func hasMedia(messages []anthropic.MessageParam) bool {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.OfImage != nil || b.OfDocument != nil {
				return true
			}
			if b.OfToolResult == nil {
				continue
			}
			for _, c := range b.OfToolResult.Content {
				if c.OfImage != nil || c.OfDocument != nil {
					return true
				}
			}
		}
	}
	return false
}

// countTokens returns the exact input token count of cp. Transient failures
// retry like a streamed turn and requeue once exhausted, so a count outage
// can neither let an oversized request through nor fail the item outright.
func (e *executor[Request, Response]) countTokens(ctx context.Context, cfg retry.RetryConfig, cp anthropic.MessageCountTokensParams) (int64, error) {
	res, err := retry.RetryWithBackoff(ctx, cfg, "count_tokens", isRetryableClaudeError, func() (*anthropic.MessageTokensCount, error) {
		return e.messages.CountTokens(ctx, cp)
	})
	if err != nil {
		if requeueErr := retry.RequeueIfRetryable(ctx, err, isRetryableClaudeError, "Claude API"); requeueErr != nil {
			return 0, fmt.Errorf("%w: %w", requeueErr, err)
		}
		return 0, fmt.Errorf("failed to count Claude request tokens: %w", err)
	}
	return res.InputTokens, nil
}

// enforceInputBudget makes params fit the input budget or rejects the turn.
// It bounds the request locally first and makes no count call when the
// bound fits. Otherwise every decision uses an exact count: reduce the
// oldest results just enough, recount, reduce every remaining eligible
// result, recount, then reject. That caps a turn at three count calls.
func (e *executor[Request, Response]) enforceInputBudget(ctx context.Context, cfg retry.RetryConfig, params *anthropic.MessageNewParams, protected func(name string) bool) error {
	budget := inputBudget(e.contextWindow, params.MaxTokens)
	cp, err := countParams(*params)
	if err != nil {
		return fmt.Errorf("failed to count Claude request tokens: %w", err)
	}
	if upperBoundTokens(cp) <= budget {
		return nil
	}
	inputTokens, err := e.countTokens(ctx, cfg, cp)
	if err != nil {
		return err
	}
	if inputTokens <= budget {
		return nil
	}

	current := inputTokens
	reducedResults := 0
	for _, all := range []bool{false, true} {
		n := reduceToolResults(params.Messages, protected, current-budget, all)
		if n == 0 {
			continue
		}
		reducedResults += n
		// Reduction replaces blocks inside the existing message slices, so
		// the count request built from the same slices sees the change.
		if current, err = e.countTokens(ctx, cfg, cp); err != nil {
			return err
		}
		if current <= budget {
			break
		}
	}

	kvs := []any{
		"model", e.modelName,
		"context_window", e.contextWindow,
		"output_headroom", params.MaxTokens,
		"budget", budget,
		"input_tokens", inputTokens,
		"reduced_input_tokens", current,
		"reduced_results", reducedResults,
	}
	if reducedResults > 0 {
		e.telemetry.RecordToolCall(ctx, "input_budget_reduction")
	}
	if current <= budget {
		clog.InfoContext(ctx, "Reduced older tool results to fit the model input budget", kvs...)
		return nil
	}
	e.telemetry.RecordToolCall(ctx, "input_budget_rejection")
	clog.WarnContext(ctx, "Rejecting Claude request over the model input budget", kvs...)
	return &InputTooLargeError{
		Model:              e.modelName,
		ContextWindow:      e.contextWindow,
		OutputHeadroom:     params.MaxTokens,
		Budget:             budget,
		InputTokens:        inputTokens,
		ReducedInputTokens: current,
		ReducedResults:     reducedResults,
	}
}
