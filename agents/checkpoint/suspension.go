/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"cmp"
	"errors"
	"fmt"
)

// Suspension is the error value an executor returns when a conversation pauses
// mid-run to be resumed later. It carries the full Envelope needed to rebuild
// the request, plus an optional human-facing Question.
//
// Modeled on workqueue's requeue-error idiom: the executor returns a Suspension
// like any other error, every metaagent/bot wrapper propagates it untouched
// (early-return on err != nil), and the reconciler at the top extracts it with
// AsSuspension. Because it travels as an ordinary error, no exported executor
// Interface ever has to grow a method to express "I paused".
type Suspension struct {
	// Envelope is the serializable capture of the paused conversation.
	Envelope

	// Question is an optional human-facing prompt describing what the agent is
	// waiting on. The question/answer lifecycle itself lives above this package
	// (checkpoint stays a pure primitive); this field is only a convenience
	// carrier so a Suspension can round-trip the prompt text.
	Question string `json:"question,omitempty"`
}

// Error implements the error interface. The pointer receiver is deliberate:
// callers must return &Suspension{...} so AsSuspension can extract it via
// errors.As, mirroring workqueue's *requeueError.
func (s *Suspension) Error() string {
	reason := cmp.Or(s.Reason, "suspended")
	return fmt.Sprintf("agent suspended at turn %d (key=%q, run=%q): %s",
		s.Turn, s.ReconcilerKey, s.RunID, reason)
}

// AsSuspension reports whether err is (or wraps) a *Suspension and, if so,
// returns it. It mirrors workqueue.GetRequeueOptions' errors.As-based extraction
// so a Suspension can surface through arbitrarily wrapped error chains.
func AsSuspension(err error) (*Suspension, bool) {
	var s *Suspension
	if errors.As(err, &s) {
		return s, true
	}
	return nil, false
}
