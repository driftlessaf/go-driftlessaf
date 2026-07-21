/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package suspend

import (
	"context"
	"time"

	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// Question is the prompt posted to the friend — the answering party, a human
// operator or another agent — when a run suspends. Its ID is a
// per-pause nonce: it binds an incoming Answer to one specific pause instance so
// a stale answer left over from an earlier pause of the same key can never be
// mistaken for the current one. The Coordinator mints a fresh ID on every
// Suspend; a QuestionStore MUST preserve it verbatim and only surface an Answer
// whose QuestionID matches the currently-pending Question.
type Question struct {
	// ID is the pause nonce. Fresh per Suspend; never reused.
	ID string `json:"id"`
	// Key is the reconciler/workqueue key the paused run belongs to.
	Key string `json:"key"`
	// RunID disambiguates successive runs of the same key (copied from the
	// suspended envelope, diagnostics only).
	RunID string `json:"run_id,omitempty"`
	// Prompt is the human-readable question text.
	Prompt string `json:"prompt"`
	// AskedAt is when the question was posted.
	AskedAt time.Time `json:"asked_at"`
}

// Answer is the friend's reply to a Question. QuestionID MUST equal the ID of
// the Question it answers so the store can reject answers bound to a stale
// pause.
type Answer struct {
	// QuestionID is the nonce of the Question this answers.
	QuestionID string `json:"question_id"`
	// Text is the raw friend answer. It flows raw all the way to the executor's
	// Resume, which owns framing (checkpoint.FramedAnswers) before the answer
	// reaches a provider payload — no layer in between frames or caps it.
	Text string `json:"text"`
	// AnsweredAt is when the friend replied.
	AnsweredAt time.Time `json:"answered_at"`
}

// QuestionStore is the transport half of the lifecycle: where a pending
// Question is posted and where its Answer eventually appears. It is deliberately
// separate from checkpoint.Store — the envelope is durable machine state, the
// question is friend-facing I/O — so the two can live in different systems (a GCS
// bucket for envelopes, a GitHub issue for questions, say).
//
// Nonce binding is the store's responsibility: Answer must only return a reply
// whose QuestionID matches the Question most recently Asked for key.
type QuestionStore interface {
	// Ask posts q as the pending question for key, replacing any previous
	// pending question (and its answer) for that key. q.ID is the fresh nonce.
	Ask(ctx context.Context, key string, q Question) error

	// Pending returns the currently pending Question for key; the bool is
	// false (with a zero Question and nil error) when none is pending. Wake
	// reads it before sweeping a dead checkpoint so the orphaned question can
	// be consumed by its nonce.
	Pending(ctx context.Context, key string) (Question, bool, error)

	// Answer returns the friend reply for key's pending question. The bool is
	// false (with a zero Answer and nil error) when the question is unanswered
	// or when the only available answer is bound to a stale question ID.
	Answer(ctx context.Context, key string) (Answer, bool, error)

	// Consume marks the question identified by questionID as fully handled for
	// key, so a later Answer for the same key returns false until a new Ask.
	// It is a no-op if questionID does not match the pending question (the
	// answer belonged to a superseded pause).
	Consume(ctx context.Context, key, questionID string) error
}

// WakeDecision is the tri-state outcome of Coordinator.Wake.
type WakeDecision int

const (
	// WakeFresh means no resumable checkpoint exists for the key: the caller
	// should run the reconcile from scratch. Returned when nothing is parked,
	// or when a parked envelope was past its deadline / drifted and has been
	// swept (envelope deleted, its orphaned question consumed best-effort).
	WakeFresh WakeDecision = iota

	// WakeRearm means a checkpoint is parked but not yet actionable: either the
	// question is still unanswered, or a concurrent waker already claimed it.
	// The caller should return a cheap RequeueAfter and mutate nothing.
	WakeRearm

	// WakeResume means the checkpoint was answered and successfully claimed
	// (Store CAS delete + question consumed). The accompanying WakeResult
	// carries the envelope and framed-ready answer to resume from. WakeResume
	// is never paired with a non-nil error: the CAS claim is the commit point,
	// so the caller must always resume from the WakeResult.
	WakeResume
)

// String implements fmt.Stringer for readable logs and test failures.
func (d WakeDecision) String() string {
	switch d {
	case WakeFresh:
		return "WakeFresh"
	case WakeRearm:
		return "WakeRearm"
	case WakeResume:
		return "WakeResume"
	default:
		return "WakeDecision(?)"
	}
}

// WakeResult carries the state a WakeResume caller needs to resume a paused run.
// It is nil for WakeFresh and WakeRearm.
type WakeResult struct {
	// Envelope is the claimed checkpoint envelope to rebuild the request from.
	Envelope *checkpoint.Envelope
	// Answer is the friend reply, passed raw to the executor's Resume — which
	// owns framing — and paired back to the suspending tool call.
	Answer Answer
	// Token is the CAS handle that was used to claim (delete) the envelope,
	// retained for telemetry and idempotency assertions.
	Token checkpoint.Token
}
