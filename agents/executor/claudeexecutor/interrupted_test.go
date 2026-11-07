/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// cancelOnCall is a "lookup" tool that cancels ctx's run with cause on its
// nth call, as a caller's deadline would while the tool runs.
func cancelOnCall(n int, cancel context.CancelCauseFunc, cause error) map[string]claudetool.Metadata[errCapResponse] {
	calls := 0
	return map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{Name: "lookup", InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}}},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				calls++
				if calls == n {
					cancel(cause)
				}
				return map[string]any{"found": calls}
			},
		},
	}
}

// TestInterruptedRunCarriesItsConversation pins that a run whose context ends
// returns the error it stopped with, still matching the context's cause,
// carrying the conversation after its last whole turn; and that a continue
// sends that conversation as it was.
func TestInterruptedRunCarriesItsConversation(t *testing.T) {
	srv := newContinueServer(t, 0)
	cause := errors.New("the caller's time ran out")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	tools := cancelOnCall(2, cancel, cause)

	_, err := srv.executor(t, 10).Execute(ctx, errCapRequest{}, tools)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want one matching context.Canceled", err)
	}
	interrupted, ok := errors.AsType[*agentexecutor.InterruptedError](err)
	if !ok || interrupted.Continuation == nil {
		t.Fatalf("error: got %T %v, want an *executor.InterruptedError with a continuation", err, err)
	}
	env := *interrupted.Continuation
	// Two turns ran: the second's tool cancelled the run, and its result
	// closed the turn before the third request failed unsent.
	if env.Turn != 1 || env.Reason != checkpoint.ReasonInterrupted || len(env.PendingToolCalls) != 0 {
		t.Errorf("Continuation: turn %d, reason %q, %d pending; want turn 1, %q, none", env.Turn, env.Reason, len(env.PendingToolCalls), checkpoint.ReasonInterrupted)
	}
	sent := srv.requests()
	// The interrupted transcript is the last request's messages plus that
	// turn's assistant message and the user message answering it.
	interruptedMessages := len(srv.messages(t, sent)) + 2

	_, err = continuer(t, srv.executor(t, 10)).Continue(t.Context(), env, cancelOnCall(0, nil, nil))
	if !errors.Is(err, agentexecutor.ErrMaxTurns) {
		t.Fatalf("Continue: got %v, want the continued run to spend the rest of its budget", err)
	}
	if got := len(srv.messages(t, sent+1)); got != interruptedMessages {
		t.Errorf("first continued request: %d messages, want %d (the interrupted transcript, sent as it was)", got, interruptedMessages)
	}
}

// TestInterruptedFirstTurnCarriesNothing pins that a run interrupted before
// any turn finished has no conversation to continue.
func TestInterruptedFirstTurnCarriesNothing(t *testing.T) {
	srv := newContinueServer(t, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := srv.executor(t, 10).Execute(ctx, errCapRequest{}, cancelOnCall(0, nil, nil))
	interrupted, ok := errors.AsType[*agentexecutor.InterruptedError](err)
	if !ok || interrupted.Continuation != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %T %v; want an *executor.InterruptedError matching context.Canceled with no continuation", err, err)
	}
}
