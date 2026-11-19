/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
)

const (
	// contextEstimateBytesPerToken converts request bytes the provider has
	// not counted yet into tokens. Two bytes per token overestimates source
	// code and prose, which tokenize at three to four.
	contextEstimateBytesPerToken = 2
	// contextCreditBytesPerToken converts the bytes a compaction removes
	// into tokens. It is the larger divisor so a saving is never overstated.
	contextCreditBytesPerToken = 4
)

// ContextBudget configures WithContextBudget.
type ContextBudget struct {
	// Tokens is the prompt size, in tokens, a request should stay under. A
	// request estimated over it is compacted.
	Tokens int64
	// LowWaterTokens is the estimate a compaction shortens results down to.
	// Zero means Tokens. A lower mark leaves room for the prompt to grow, so
	// the next compaction is that many tokens of new results away.
	LowWaterTokens int64
	// KeepRecentTurns is how many of the most recent assistant turns, before
	// the newest one, keep their tool results whole while shortening every
	// older result reaches LowWaterTokens. When it does not, these results
	// are shortened too, largest first, until the estimate reaches the mark.
	// The newest turn's results are always whole: the model has not read
	// them yet.
	KeepRecentTurns int
	// Protect, when set, reports the tool calls whose results are never
	// shortened, by tool name and JSON input. The submit and suspend tools
	// are always protected.
	Protect func(tool string, input json.RawMessage) bool
	// OnCompact, when set, is called on the conversation's goroutine after
	// each compaction.
	OnCompact func(ContextCompaction)
}

// ContextCompaction describes one compaction of a conversation.
type ContextCompaction struct {
	// Turn is the turn whose request was compacted.
	Turn int
	// EstimatedTokens is the request's estimated prompt before compaction.
	EstimatedTokens int64
	// ReducedResults is how many tool results were shortened.
	ReducedResults int
	// RecentResults is how many of them were results of the
	// ContextBudget.KeepRecentTurns turns.
	RecentResults int
	// SavedBytes is how many bytes of tool result text were removed.
	SavedBytes int64
}

// contextAnchor is the provider's count for the last answered request, from
// which the next request is estimated.
type contextAnchor struct {
	// tokens bounds the answered request plus the assistant message that
	// answered it (nextPromptTokens). Zero means no request was answered.
	tokens int64
	// messages is how many messages the answered request carried.
	messages int
}

// estimate returns the estimated prompt tokens of a request carrying
// messages. Only the messages appended since the anchored request are
// estimated from their bytes, and the assistant message that answered it is
// skipped because the anchor's output tokens cover it.
func (a contextAnchor) estimate(messages []anthropic.MessageParam) int64 {
	appended := messages[min(a.messages, len(messages)):]
	if len(appended) > 0 && appended[0].Role == anthropic.MessageParamRoleAssistant {
		appended = appended[1:]
	}
	if len(appended) == 0 {
		return a.tokens
	}
	return a.tokens + estimateBytes(appended)
}

// estimateBytes estimates the tokens of v from its JSON encoding. A value
// that does not encode estimates as zero; the request would fail to send.
func estimateBytes(v any) int64 {
	body, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return (int64(len(body)) + contextEstimateBytesPerToken - 1) / contextEstimateBytesPerToken
}

// plannedReduction is one tool result a compaction would shorten.
type plannedReduction struct {
	at     tailPosition
	result *anthropic.ToolResultBlockParam
	saved  int
}

// compactionPlan lists the tool results that can be shortened, with their
// shortened copies. Each list is oldest first.
type compactionPlan struct {
	// older are the results before the keep window.
	older []plannedReduction
	// recent are the results of the keep window's turns.
	recent []plannedReduction
	// unread are the results of the newest assistant turn. No compaction
	// shortens them until the model has answered them.
	unread []plannedReduction
}

// planCompaction lists every tool result a compaction of messages could
// shorten. It changes nothing.
//
// Eligibility is reduceToolResults', less the results protect reports true
// for. The keep window is the keepRecentTurns assistant turns before the
// newest one.
func planCompaction(messages []anthropic.MessageParam, keepRecentTurns int, protect func(tool string, input json.RawMessage) bool) compactionPlan {
	// assistants holds the newest keepRecentTurns+1 assistant messages,
	// newest first.
	assistants := make([]int, 0, keepRecentTurns+1)
	for i, m := range slices.Backward(messages) {
		if m.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		assistants = append(assistants, i)
		if len(assistants) > keepRecentTurns {
			break
		}
	}
	if len(assistants) == 0 {
		return compactionPlan{}
	}
	last, cut := assistants[0], 0
	if len(assistants) > keepRecentTurns {
		cut = assistants[keepRecentTurns]
	}

	uses := make(map[string]*anthropic.ToolUseBlockParam)
	for _, m := range messages {
		if m.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.OfToolUse != nil {
				uses[b.OfToolUse.ID] = b.OfToolUse
			}
		}
	}

	var plan compactionPlan
	for i, m := range messages {
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for b := range m.Content {
			tr := m.Content[b].OfToolResult
			if tr == nil {
				continue
			}
			use, ok := uses[tr.ToolUseID]
			if !ok {
				continue
			}
			// An input that does not encode cannot be shown to protect, so
			// its result stays whole.
			input, err := json.Marshal(use.Input)
			if err != nil || protect(use.Name, input) {
				continue
			}
			nb, saved, ok := reducedToolResult(tr, use.Name)
			if !ok {
				continue
			}
			r := plannedReduction{at: tailPosition{message: i, block: b}, result: nb, saved: saved}
			switch {
			case i < cut:
				plan.older = append(plan.older, r)
			case i < last:
				plan.recent = append(plan.recent, r)
			default:
				plan.unread = append(plan.unread, r)
			}
		}
	}
	return plan
}

// reach is the most tokens a compaction of p can be credited with removing.
func (p compactionPlan) reach() int64 {
	var saved int64
	for _, r := range slices.Concat(p.older, p.recent) {
		saved += int64(r.saved)
	}
	return saved / contextCreditBytesPerToken
}

// take returns the reductions a compaction applies to remove excess tokens,
// and how many of them are recent results.
//
// It takes every older result, whatever the excess. The rewritten messages
// are written to the prompt cache again, so shortening one result per turn
// would pay that write on every turn. It then takes recent results, largest
// saving first, only until the credited saving covers the excess: one
// oversized result is then the only one the keep window loses.
//
// rest is the first result left whole that a later compaction could shorten,
// or nil when there is none.
func (p compactionPlan) take(excess int64) (taken []plannedReduction, recent int, rest *tailPosition) {
	taken = slices.Clone(p.older)
	var saved int64
	for _, r := range taken {
		saved += int64(r.saved)
	}
	bySaving := slices.Clone(p.recent)
	slices.SortStableFunc(bySaving, func(a, b plannedReduction) int { return cmp.Compare(b.saved, a.saved) })
	for recent < len(bySaving) && saved/contextCreditBytesPerToken < excess {
		saved += int64(bySaving[recent].saved)
		recent++
	}
	taken = append(taken, bySaving[:recent]...)

	for _, r := range slices.Concat(bySaving[recent:], p.unread) {
		if rest == nil || r.at.before(*rest) {
			rest = &r.at
		}
	}
	return taken, recent, rest
}

// compactToBudget shortens tool results when the request is estimated over
// the context budget, and returns the anchor to estimate later requests from.
//
// The request is brought as close to ContextBudget.LowWaterTokens as the
// eligible results allow, and is sent whatever its size. See
// compactionPlan.take for which results are shortened. A request that only
// its unread results keep over the mark loses its older results and keeps its
// window.
//
// Shortened results are not eligible again, so every compaction rewrites
// only blocks after the ones the previous compaction left whole.
// tailBreakpoints.repin keeps the prompt cache before them readable.
//
// A conversation with no answered request has no provider count to estimate
// from, and is not compacted: its bytes alone overstate the prompt by up to
// half, which would shorten results a request under the budget could keep.
func (e *executor[Request, Response]) compactToBudget(ctx context.Context, turn int, params *anthropic.MessageNewParams, anchor contextAnchor, protectedResult func(name string) bool, tail *tailBreakpoints) contextAnchor {
	budget := e.contextBudget
	if anchor.tokens == 0 {
		return anchor
	}
	estimated := anchor.estimate(params.Messages)
	if estimated <= budget.Tokens {
		return anchor
	}
	plan := planCompaction(params.Messages, budget.KeepRecentTurns, func(tool string, input json.RawMessage) bool {
		return protectedResult(tool) || (budget.Protect != nil && budget.Protect(tool, input))
	})
	excess := estimated - cmp.Or(budget.LowWaterTokens, budget.Tokens)
	// Results the model has not read cannot be shortened on this request and
	// can on the next. When they are all that keeps this request over the
	// mark, shortening the keep window would not bring it under, and the
	// next request will not need it.
	if reach := plan.reach(); excess > reach && excess-(estimated-anchor.tokens) <= reach {
		excess = 0
	}
	taken, recent, rest := plan.take(excess)
	if len(taken) == 0 {
		return anchor
	}
	var saved int64
	first := taken[0].at
	for _, r := range taken {
		params.Messages[r.at.message].Content[r.at.block].OfToolResult = r.result
		saved += int64(r.saved)
		if r.at.before(first) {
			first = r.at
		}
	}
	if e.cacheControl && !e.disableTailCacheControl {
		tail.repin(params.Messages, first, rest)
	}
	e.telemetry.RecordToolCall(ctx, "context_budget_compaction")
	clog.InfoContext(ctx, "Shortened tool results to bring the request toward the context budget",
		"model", e.modelName,
		"turn", turn,
		"budget", budget.Tokens,
		"low_water", budget.LowWaterTokens,
		"estimated_tokens", estimated,
		"reduced_results", len(taken),
		"recent_results", recent,
		"saved_bytes", saved)
	if budget.OnCompact != nil {
		budget.OnCompact(ContextCompaction{Turn: turn, EstimatedTokens: estimated, ReducedResults: len(taken), RecentResults: recent, SavedBytes: saved})
	}
	// Without the credit, a turn that fails before the provider counts the
	// compacted request would estimate the next one as if nothing had been
	// removed.
	anchor.tokens = max(anchor.tokens-saved/contextCreditBytesPerToken, 1)
	return anchor
}
