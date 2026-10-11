/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package executor

import (
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// ErrMaxTurns reports that an agent run stopped because it reached its
// conversation-turn budget before the agent submitted a result. Every executor
// wraps it, so a caller can recognize the condition with errors.Is regardless
// of which provider ran the agent and decide what to do with the run's partial
// work.
var ErrMaxTurns = errors.New("agent exceeded maximum conversation turns")

// MaxTurnsError is the ErrMaxTurns an executor returns when it can say more
// than the sentinel does. It unwraps to ErrMaxTurns and reads the same, so a
// caller that matches with errors.Is is unaffected; one that wants the run's
// partial work extracts it with errors.AsType.
type MaxTurnsError struct {
	// Turns is the turn budget the run enforced. For a resumed or continued
	// run that is the budget of that run alone, not of the whole conversation.
	Turns int

	// Continuation is the conversation as it stood after its last turn, in
	// the envelope shape a resume reads, so an executor that can continue a
	// conversation picks it up at a larger budget instead of starting over.
	// It carries no pending tool calls and no remaining budget: nothing is
	// parked, and the budget is the continuing executor's. Nil means the
	// executor that ran out cannot continue the conversation.
	//
	// It carries no Deadline either, because it is meant to be continued in
	// the same process. A caller that persists it stamps its own Deadline,
	// which ValidateForContinue then enforces.
	//
	// It is excluded from JSON: its provider state is the whole transcript
	// (system prompt, tool results, repository content), which must not leak
	// into a status record that marshals the error.
	Continuation *checkpoint.Envelope `json:"-"`
}

// Error reads as the wrapped sentinel with the enforced budget appended.
func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("%v (%d)", ErrMaxTurns, e.Turns)
}

// Unwrap returns ErrMaxTurns.
func (e *MaxTurnsError) Unwrap() error {
	return ErrMaxTurns
}

// ErrInputTooLarge reports that a model request exceeded the input budget of
// the model's serving context window and was rejected before the provider was
// called. Resending the same conversation fails the same way, so callers
// should treat it as non-retriable. Executors that enforce an input budget
// wrap it, so a caller can recognize the condition with errors.Is without
// importing a provider-specific error type.
var ErrInputTooLarge = errors.New("agent request exceeds model input budget")
