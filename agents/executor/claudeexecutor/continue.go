/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"fmt"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/chainguard-dev/clog"
)

// Continuer is the capability interface for continuing a conversation that
// spent its turn budget, instead of starting it over: the Continuation an
// exhausted run carries on its executor.MaxTurnsError. Like Resumer it is kept
// off Interface, so existing consumers compile unmodified; a caller that
// continues type-asserts the Interface returned by New.
type Continuer[Request promptbuilder.Bindable, Response any] interface {
	// Continue picks the conversation in env up after its last turn and runs
	// it on this executor's turn budget, which counts the whole conversation:
	// an executor built WithMaxTurns(n) runs n-(env.Turn+1) more turns, so a
	// continued conversation spends at most what one run at n spends. tools
	// must be the set the exhausted run had, and the executor's configuration
	// that run's but for the budget. It returns checkpoint.ErrConfigDrift
	// (wrapped) when the live configuration no longer matches the envelope,
	// and a plain error when the budget leaves no turn to run.
	Continue(ctx context.Context, env checkpoint.Envelope, tools map[string]claudetool.Metadata[Response]) (Response, error)
}

var _ Continuer[promptbuilder.Bindable, any] = (*executor[promptbuilder.Bindable, any])(nil)

// Continue implements Continuer. It shares Resume's restore and run halves
// (restoreParams, runRestored); what differs is that nothing is pending, so no
// answer is paired and the captured request goes out as it is, and that the
// budget is the live executor's alone.
func (e *executor[Request, Response]) Continue(
	ctx context.Context,
	env checkpoint.Envelope,
	tools map[string]claudetool.Metadata[Response],
) (Response, error) {
	var zero Response
	digest, err := e.configDigest(tools)
	if err != nil {
		return zero, err
	}
	if err := checkpoint.ValidateForContinue(env, suspendProviderName, e.modelName, digest, time.Now()); err != nil {
		return zero, err
	}
	ran := env.Turn + 1
	budget := e.maxTurns - ran
	if budget <= 0 {
		return zero, fmt.Errorf("continue: the conversation already ran %d turns, and this executor's budget of %d leaves none", ran, e.maxTurns)
	}

	params, err := e.restoreParams(env, tools)
	if err != nil {
		return zero, fmt.Errorf("continue: %w", err)
	}
	// The captured run ended on the user message its last turn appended, so
	// the request is ready to send. A transcript ending anywhere else was not
	// captured at a turn boundary.
	if _, err := lastUserMessage(&params); err != nil {
		return zero, fmt.Errorf("continue: %w", err)
	}
	state, err := decodeLoopState(ctx, env.LoopState)
	if err != nil {
		return zero, err
	}

	clog.InfoContext(ctx, "Continuing turn-exhausted Claude agent execution",
		"turn", env.Turn, "turns", budget, "max_turns", e.maxTurns, "linked_trace", env.TraceID)
	return e.runRestored(ctx, env, params, tools, state, budget, continueTracePrompt(env))
}

// continueTracePrompt is the prompt string recorded on a continued run's
// trace, the counterpart of resumeTracePrompt.
func continueTracePrompt(env checkpoint.Envelope) string {
	return fmt.Sprintf("continue turn-exhausted agent after turn %d (reason=%q)", env.Turn, env.Reason)
}
