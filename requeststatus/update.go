/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

// Update is one host-independent statement of request state. It carries
// exactly one of Phase or Outcome, never both and never neither; see
// [Validator.Validate] for the full coherence contract.
type Update struct {
	// Phase is set for an active or stopped-without-outcome request. Empty
	// when Outcome is set.
	Phase Phase
	// Outcome is set for a terminal, non-failure result. Empty when Phase
	// is set.
	Outcome Outcome

	// Activity refines PhaseRunning with the orchestrator's current step.
	// Empty unless Phase is PhaseRunning.
	Activity Activity
	// Attempt is the one-based diagnostics-loop iteration for Activity. It
	// is positive when Activity is set and zero otherwise; it is not the
	// changemanager commit count.
	Attempt int

	// Reason classifies why Phase or Outcome holds this value. Required
	// and validated against a registered set for PhaseRunning and
	// PhaseWaiting; required and open for PhaseNeedsYou and PhaseFailed;
	// fixed per Outcome. See [Validator.Validate].
	Reason string

	// Summary is a short, requester-facing explanation, bounded and
	// sanitized by the renderer that builds a Document from this Update.
	Summary string
	// Details are additional bullet-point explanations, bounded and
	// sanitized the same way as Summary.
	Details []string

	// Change links the proposed change a requester can follow. It is
	// presentation only and never selects a Role or a Surface.
	Change ChangeLink
}

// EntryKey is the host-independent logical identity of one bot-owned status
// entry, for example "manifest-gen:request-status". Each [Surface] adapter
// encodes this key in its own host's terms; the key itself names no host
// object.
type EntryKey string
