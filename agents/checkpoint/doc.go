/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

/*
Package checkpoint defines the provider-neutral suspend/resume primitive for
DriftlessAF agents: a serializable Envelope that captures everything needed
to rebuild a paused conversation, a Suspension error value that travels out
of an executor's Execute loop the same way workqueue's requeue errors do, and
a Store contract (with compare-and-swap Token semantics) for durably parking
exactly one envelope per {identity}/{key}.

The package is a pure primitive: it knows nothing about human questions, the
workqueue, or any specific LLM provider. The three provider SDK payloads are
carried opaquely in Envelope.ProviderState as json.RawMessage so a microVM
checkpointer or a GCS-backed store can reuse the exact same envelope shape.

# Overview

The core types compose into a suspend/park/wake/resume lifecycle:

  - Envelope: versioned, serializable capture of a paused conversation —
    provider identity, config digest, turn budget, pending tool calls, and
    the raw provider request payload.
  - Suspension: an error-shaped carrier for an Envelope. Executors return
    &Suspension{...} like any other error and callers extract it with
    AsSuspension, so no executor interface ever grows a method for pausing.
  - Store: the durable home for parked envelopes, with Load returning a CAS
    Token that Delete uses as a claim-once primitive (ErrTokenMismatch
    signals a lost race).
  - FrameAnswer / FramedAnswers: wrap human answers in distinctive
    delimiters, substitute a placeholder for empty answers, and cap length
    on a UTF-8 boundary before they are injected as tool results.

# Fail-Closed Validation

Validation happens at both ends of the pause. Envelope.Validate rejects an
unpairable or unreplayable envelope at suspend time — including one with no
remaining turn budget, which could never pass the resume gate — before a
checkpoint is persisted and a human spends time answering. ValidateForResume gates the
wake side: version, provider, model, and config digest (see DigestJSON) must
all match the live executor — the digest is required, so an empty digest on
either side fails closed rather than vacuously matching — turn budget must
remain, and any park-time Deadline must not have passed; drift surfaces as
ErrConfigDrift so callers rebuild from scratch instead of resuming against
stale state.

# Usage

An executor suspends by returning a Suspension from its Execute loop:

	return &checkpoint.Suspension{...} // typically via NewAskHumanSuspension

The reconciler at the top extracts it, parks the envelope, and requeues:

	if s, ok := checkpoint.AsSuspension(err); ok {
		if err := store.Save(ctx, key, &s.Envelope); err != nil {
			return err
		}
		// ask the human s.Question, then requeue to wake later.
	}

On wake, the resumer claims the envelope with the CAS token and replays it:

	env, tok, ok, err := store.Load(ctx, key)
	// validate with checkpoint.ValidateForResume, frame the human answer
	// with checkpoint.FramedAnswers, then claim via store.Delete(ctx, key, tok).

Store implementations live in the memstore (in-memory), jsonlstore
(append-only local file), and future GCS-backed subpackages; all are held to
the same contract by the storetest conformance suite.
*/
package checkpoint
