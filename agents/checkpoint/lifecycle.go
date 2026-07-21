/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Provider identifiers stamped into Envelope.Provider by the executors and
// matched fail-closed on resume. Wakers that route on provider (a dispatcher
// picking which executor to rebuild) share these instead of re-typing the
// strings.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderGoogle    = "google"
)

// ReasonAwaitingAnswer is the Envelope.Reason for a suspension triggered
// by a held-out ask-a-friend tool call.
const ReasonAwaitingAnswer = "awaiting answer"

// StatusAwaitingAnswer is the status value in the synthetic tool result
// recorded for the suspend call itself (the call is intercepted, never
// dispatched, so the transcript needs a placeholder result to stay paired).
const StatusAwaitingAnswer = "awaiting_answer"

// DefaultAnswerMaxBytes caps a framed friend answer on the resume path. One
// policy value for the whole lifecycle: FrameAnswer applies it inside the
// executors' Resume, which own framing (callers pass answers raw).
const DefaultAnswerMaxBytes = 16384

// questionInputKey is the tool-input property conventionally carrying the
// friend-facing question text in an ask-a-friend suspend call.
const questionInputKey = "question"

// NewAskAFriendSuspension assembles the Suspension an executor returns when the
// model calls its held-out ask-a-friend tool: it stamps the schema version,
// clamps the remaining-turns budget (turn is 0-based, so turn+1 turns are
// consumed), records the suspend call as the sole pending tool call, and
// derives the friend-facing Question from the call's input. Executors supply
// only the provider-typed pieces: the marshaled ProviderState/LoopState and
// their own config digest. A suspension fired on the final turn yields
// RemainingTurns 0; Validate rejects that envelope at park time (there is no
// budget left to resume into), so the run fails before a friend is asked.
func NewAskAFriendSuspension(provider, model, configDigest string, turn, maxTurns int, call PendingToolCall, providerState, loopState json.RawMessage, traceID string) *Suspension {
	return &Suspension{
		Envelope: Envelope{
			Version:          EnvelopeVersion,
			Provider:         provider,
			Model:            model,
			ConfigDigest:     configDigest,
			Turn:             turn,
			RemainingTurns:   max(maxTurns-(turn+1), 0),
			Reason:           ReasonAwaitingAnswer,
			PendingToolCalls: []PendingToolCall{call},
			ProviderState:    providerState,
			LoopState:        loopState,
			TraceID:          traceID,
		},
		Question: QuestionFromPending([]PendingToolCall{call}),
	}
}

// QuestionFromPending extracts the friend-facing question text from the first
// pending tool call whose input carries a "question" string property (the
// ask-a-friend tool convention, see questionInputKey). It returns "" when no
// pending call carries one, so callers can fall back to their own prompt.
func QuestionFromPending(calls []PendingToolCall) string {
	for _, pc := range calls {
		var args map[string]any
		if err := json.Unmarshal(pc.InputJSON, &args); err != nil {
			continue
		}
		if q, ok := args[questionInputKey].(string); ok && q != "" {
			return q
		}
	}
	return ""
}

// Validate reports whether the envelope is complete enough to park: an
// unpairable, unreplayable, or unverifiable envelope must fail at suspend
// time — before a checkpoint is persisted and a friend spends time answering —
// not at resume. That includes an exhausted turn budget: ValidateForResume
// rejects RemainingTurns <= 0, so an envelope parked without budget could
// never wake, and the two gates must agree.
func (e *Envelope) Validate() error {
	if e.Version != EnvelopeVersion {
		return fmt.Errorf("checkpoint: envelope version %d, want %d", e.Version, EnvelopeVersion)
	}
	if len(e.ProviderState) == 0 {
		return errors.New("checkpoint: envelope has no provider state")
	}
	if e.ConfigDigest == "" {
		return errors.New("checkpoint: envelope has no config digest; resume could not verify config drift")
	}
	if e.RemainingTurns <= 0 {
		return errors.New("checkpoint: envelope has no remaining turn budget; resume would always fail its turn gate")
	}
	if len(e.PendingToolCalls) == 0 {
		return errors.New("checkpoint: envelope has no pending tool calls")
	}
	for i, pc := range e.PendingToolCalls {
		if pc.ID == "" {
			return fmt.Errorf("checkpoint: pending tool call %d (%s) has an empty ID; resume could not pair an answer", i, pc.Name)
		}
	}
	return nil
}

// ValidateForResume is the fail-closed gate every executor Resume runs before
// replaying a parked envelope: the schema version, provider, model, and config
// digest must all match the live executor, turn budget must remain, and any
// park-time Deadline must not have passed at now. The digest is required on
// both sides — an empty envelope or live digest is rejected rather than
// vacuously matching another empty string, so the gate cannot be silently
// disabled by a caller that never stamped one. Mismatches return
// ErrConfigDrift (wrapped) so the caller rebuilds from scratch rather than
// resuming against stale state; an exhausted budget or expired deadline is a
// plain error — there is nothing valid to rebuild toward.
func ValidateForResume(env Envelope, provider, model, liveDigest string, now time.Time) error {
	if env.Version != EnvelopeVersion {
		return fmt.Errorf("%w: envelope version %d, executor speaks %d", ErrConfigDrift, env.Version, EnvelopeVersion)
	}
	if env.Provider != provider {
		return fmt.Errorf("%w: envelope provider %q, executor is %q", ErrConfigDrift, env.Provider, provider)
	}
	if env.Model != model {
		return fmt.Errorf("%w: envelope model %q, executor uses %q", ErrConfigDrift, env.Model, model)
	}
	if env.ConfigDigest == "" || liveDigest == "" {
		return fmt.Errorf("%w: config digest missing (envelope %q, live %q); an unverifiable envelope must not resume", ErrConfigDrift, env.ConfigDigest, liveDigest)
	}
	if env.ConfigDigest != liveDigest {
		return fmt.Errorf("%w: envelope config digest %s, live config digest %s", ErrConfigDrift, env.ConfigDigest, liveDigest)
	}
	if env.RemainingTurns <= 0 {
		return errors.New("checkpoint: envelope has no remaining turn budget")
	}
	if !env.Deadline.IsZero() && now.After(env.Deadline) {
		return fmt.Errorf("checkpoint: envelope deadline %s passed (now %s)",
			env.Deadline.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	return nil
}

// FramedAnswer is a friend answer framed for injection, paired to the pending
// tool call it answers. The executor maps it into its provider message shape.
type FramedAnswer struct {
	// ID is the pending tool call's provider identifier.
	ID string
	// Name is the pending tool call's tool name.
	Name string
	// Text is the framed answer body (delimited, capped, empty-substituted).
	Text string
}

// FramedAnswers frames one answer per pending tool call, in envelope order. A
// pending call with no supplied answer gets the explicit empty-answer
// placeholder rather than being skipped, so no provider request ever carries
// an unanswered tool call. maxBytes <= 0 applies DefaultAnswerMaxBytes.
//
// Pairing an answer to every pending call cannot fabricate a result for a
// privileged tool: only held-out ask-a-friend calls can be pending — dispatched
// siblings quiesce to real transcript results before park (see
// PendingToolCall) — and FrameAnswer delimits every body as quoted friend
// text, so the model never reads it as native output of the named tool.
func FramedAnswers(pending []PendingToolCall, answers map[string]string, maxBytes int) ([]FramedAnswer, error) {
	if len(pending) == 0 {
		return nil, errors.New("checkpoint: envelope has no pending tool calls to answer")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultAnswerMaxBytes
	}
	framed := make([]FramedAnswer, 0, len(pending))
	for _, pc := range pending {
		if pc.ID == "" {
			return nil, errors.New("checkpoint: pending tool call has an empty ID; cannot pair an answer")
		}
		framed = append(framed, FramedAnswer{
			ID:   pc.ID,
			Name: pc.Name,
			Text: FrameAnswer(answers[pc.ID], maxBytes),
		})
	}
	return framed, nil
}
