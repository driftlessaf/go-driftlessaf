/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// loopingExecutor builds an executor over a server whose every turn calls
// "lookup", a tool the runs below never register: each turn draws an
// unknown-tool result and the loop goes on, so a run can only end by spending
// its turn budget. requests reports how many model requests the server saw.
func loopingExecutor(t *testing.T, maxTurns int) (exec claudeexecutor.Interface[errCapRequest, errCapResponse], requests func() int) {
	t.Helper()
	var seen int
	srv := newValidatingAnthropicServer(t, func(reqNum int, _ []byte) []string {
		seen = reqNum
		return wrapTurn(fmt.Sprintf("msg_loop_%d", reqNum),
			toolUseBlockSSE(t, 0, fmt.Sprintf("toolu_loop_%d", reqNum), "lookup",
				map[string]any{"reasoning": "keep digging"})...)
	})
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err = claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec, func() int { return seen }
}

// exhaustedContinuation asserts err is the turn exhaustion of a run that
// enforced budget, reading exactly as the bare sentinel always has, and
// returns the conversation it carries.
func exhaustedContinuation(t *testing.T, err error, budget int) *checkpoint.Envelope {
	t.Helper()
	if !errors.Is(err, agentexecutor.ErrMaxTurns) {
		t.Fatalf("error: got %v, want one wrapping ErrMaxTurns", err)
	}
	if got, want := err.Error(), fmt.Sprintf("agent exceeded maximum conversation turns (%d)", budget); got != want {
		t.Errorf("error text: got %q, want %q", got, want)
	}
	exhausted, ok := errors.AsType[*agentexecutor.MaxTurnsError](err)
	if !ok {
		t.Fatalf("error: got %T, want a *executor.MaxTurnsError", err)
	}
	if exhausted.Turns != budget {
		t.Errorf("Turns: got %d, want %d", exhausted.Turns, budget)
	}
	if exhausted.Continuation == nil {
		t.Fatal("Continuation: got nil, want the exhausted conversation")
	}
	return exhausted.Continuation
}

// lastMessage decodes the envelope's provider state and returns its final
// message.
func lastMessage(t *testing.T, env *checkpoint.Envelope) anthropic.MessageParam {
	t.Helper()
	var params anthropic.MessageNewParams
	if err := json.Unmarshal(env.ProviderState, &params); err != nil {
		t.Fatalf("unmarshal provider state: %v", err)
	}
	if len(params.Messages) == 0 {
		t.Fatal("provider state carries no messages")
	}
	return params.Messages[len(params.Messages)-1]
}

// toolResultIDs lists the tool_use ids msg's tool_result blocks answer.
func toolResultIDs(msg anthropic.MessageParam) []string {
	var ids []string
	for _, block := range msg.Content {
		if block.OfToolResult != nil {
			ids = append(ids, block.OfToolResult.ToolUseID)
		}
	}
	return ids
}

// TestMaxTurnsErrorCarriesConversation pins what an exhausted run hands back:
// the same error text and errors.Is match it always had, plus the conversation
// after its last turn in the shape Resume reads, ending on the user message
// that answers the last turn's tool call, so it can be sent again as it is.
func TestMaxTurnsErrorCarriesConversation(t *testing.T) {
	const maxTurns = 2
	exec, requests := loopingExecutor(t, maxTurns)

	_, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})
	env := exhaustedContinuation(t, err, maxTurns)

	if requests() != maxTurns {
		t.Errorf("provider requests: got %d, want %d", requests(), maxTurns)
	}
	if env.Version != checkpoint.EnvelopeVersion {
		t.Errorf("Version: got %d, want %d", env.Version, checkpoint.EnvelopeVersion)
	}
	if env.Provider != checkpoint.ProviderAnthropic {
		t.Errorf("Provider: got %q, want %q", env.Provider, checkpoint.ProviderAnthropic)
	}
	if env.Model != defaultResumeModel {
		t.Errorf("Model: got %q, want %q", env.Model, defaultResumeModel)
	}
	if want := liveConfigDigest(t); env.ConfigDigest != want {
		t.Errorf("ConfigDigest: got %q, want the live digest %q", env.ConfigDigest, want)
	}
	if got, want := env.Turn, maxTurns-1; got != want {
		t.Errorf("Turn: got %d, want %d (the last turn that ran)", got, want)
	}
	if env.RemainingTurns != 0 {
		t.Errorf("RemainingTurns: got %d, want 0 (the budget belongs to whoever continues)", env.RemainingTurns)
	}
	if env.Reason != checkpoint.ReasonTurnsExhausted {
		t.Errorf("Reason: got %q, want %q", env.Reason, checkpoint.ReasonTurnsExhausted)
	}
	if len(env.PendingToolCalls) != 0 {
		t.Errorf("PendingToolCalls: got %v, want none (nothing is parked)", env.PendingToolCalls)
	}
	if env.TraceID == "" {
		t.Error("TraceID: got empty, want the exhausted run's trace")
	}
	if len(env.LoopState) != 0 {
		t.Errorf("LoopState: got %s, want none (the run tripped no per-run bound)", env.LoopState)
	}

	last := lastMessage(t, env)
	if last.Role != anthropic.MessageParamRoleUser {
		t.Errorf("last message role: got %q, want user", last.Role)
	}
	if got, want := toolResultIDs(last), []string{fmt.Sprintf("toolu_loop_%d", maxTurns)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("last message answers %v, want %v (the final turn's tool call)", got, want)
	}
}

// TestResumedMaxTurnsErrorCountsTheWholeConversation pins Turn as a
// conversation-wide index: a resumed run that spends its remaining budget
// reports the last turn of the whole conversation, while Turns reports the
// budget that run alone enforced.
func TestResumedMaxTurnsErrorCountsTheWholeConversation(t *testing.T) {
	const (
		parkedTurn = 3
		remaining  = 2
	)
	exec, requests := loopingExecutor(t, 10)
	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor does not satisfy Resumer")
	}
	providerState, _ := staleCacheProviderState(t)
	env := checkpoint.Envelope{
		Version:        checkpoint.EnvelopeVersion,
		Provider:       checkpoint.ProviderAnthropic,
		Model:          defaultResumeModel,
		ConfigDigest:   liveConfigDigest(t),
		Turn:           parkedTurn,
		RemainingTurns: remaining,
		Reason:         checkpoint.ReasonAwaitingAnswer,
		PendingToolCalls: []checkpoint.PendingToolCall{{
			ID:   "toolu_ask",
			Name: askAFriendToolName,
		}},
		ProviderState: providerState,
	}

	_, err := resumer.Resume(t.Context(), env, map[string]string{"toolu_ask": "yes"},
		map[string]claudetool.Metadata[errCapResponse]{})
	got := exhaustedContinuation(t, err, remaining)

	if requests() != remaining {
		t.Errorf("provider requests: got %d, want %d", requests(), remaining)
	}
	if want := parkedTurn + remaining; got.Turn != want {
		t.Errorf("Turn: got %d, want %d (turns %d..%d ran after the park at %d)", got.Turn, want, parkedTurn+1, want, parkedTurn)
	}
	if got.Reason != checkpoint.ReasonTurnsExhausted || len(got.PendingToolCalls) != 0 {
		t.Errorf("envelope: reason %q with %d pending calls, want %q with none", got.Reason, len(got.PendingToolCalls), checkpoint.ReasonTurnsExhausted)
	}
}
