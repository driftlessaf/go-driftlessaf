/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package suspend orchestrates the question/answer lifecycle and halt/wake
// loop that sits on top of the checkpoint primitive. It turns an executor's
// checkpoint.Suspension (an error value carrying a serializable Envelope) into
// a durable pause: the envelope is parked in a checkpoint.Store, a human-facing
// Question is posted to a QuestionStore under a fresh nonce, and the reconciler
// is asked to come back later via a workqueue requeue — no live process is held
// across the wait. The wait itself is always bounded: an envelope parked
// without a deadline is stamped with the Coordinator's DefaultParkDeadline
// (14 days unless configured), and Wake's dead-checkpoint sweep retires any
// park whose deadline has lapsed unanswered.
//
// The Coordinator is the single entry point. Suspend halts a run; Wake is the
// tri-state re-entry probe a reconciler calls at the top of every reconcile:
//
//	WakeFresh  — nothing parked (or the checkpoint expired/drifted): run from scratch.
//	WakeRearm  — parked but no answer yet (or a duplicate waker won the claim):
//	             return a cheap RequeueAfter and touch nothing.
//	WakeResume — parked and answered: the envelope has been claimed (Store CAS
//	             delete) and the question consumed; resume with the raw answer
//	             (the executor's Resume owns framing).
//
// checkpoint stays a pure primitive: it knows nothing about questions, nonces,
// or the workqueue. All of that lives here.
package suspend
