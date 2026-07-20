/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
)

// ValidateSuspendToolName is the construction-time guard shared by every
// executor's suspend option: the suspend tool must have a name, and it must
// not collide with the terminal submit tool — a collision would make the
// pause check intercept every submission so the run could never terminate.
func ValidateSuspendToolName(suspendName, submitName string) error {
	if suspendName == "" {
		return fmt.Errorf("suspend tool must have a name")
	}
	if submitName != "" && suspendName == submitName {
		return fmt.Errorf("suspend tool %q collides with the submit tool name; the run could never terminate", suspendName)
	}
	return nil
}

// HeldOutPartition builds the per-run tool partition every executor's turn
// loop uses: isSubmit and isSuspend route by name with identical
// not-shadowed-by-caller-tool semantics (see SubmitPredicate), and heldOut is
// their union — the set DispatchToolCalls holds out of the concurrent pool so
// a held-out call runs only after every sibling handler has finished and its
// real result is in the transcript. It fails when the suspend tool is
// shadowed by a caller-registered tool of the same name: silently routing the
// call to the caller's handler would no-op the operator's intent to pause.
func HeldOutPartition[Meta any](tools map[string]Meta, submitName string, submitConfigured bool, suspendName string, suspendConfigured bool) (isSubmit, isSuspend, heldOut func(name string) bool, err error) {
	if suspendConfigured && suspendName != "" {
		if _, shadowed := tools[suspendName]; shadowed {
			return nil, nil, nil, fmt.Errorf("suspend tool %q collides with a caller-registered tool of the same name", suspendName)
		}
	}
	isSubmit = SubmitPredicate(tools, submitName, submitConfigured)
	isSuspend = SubmitPredicate(tools, suspendName, suspendConfigured)
	held := make(map[string]struct{}, 2)
	if isSubmit(submitName) {
		held[submitName] = struct{}{}
	}
	if isSuspend(suspendName) {
		held[suspendName] = struct{}{}
	}
	heldOut = func(name string) bool { _, ok := held[name]; return ok }
	return isSubmit, isSuspend, heldOut, nil
}

// FailTurnUnlessSuspended records err on the turn unless it is a
// *checkpoint.Suspension: a suspension is an intentional halt, not a failure,
// so the turn must not be marked Failed nor its span stamped Error (the
// trace-level disposition is set by Trace.Suspend on the suspend path). Every
// executor's per-turn error defer routes through this one carve-out so the
// three backends cannot drift on what counts as a failed turn.
func FailTurnUnlessSuspended[T any](llmTurn *agenttrace.LLMTurn[T], err error) {
	if err == nil {
		return
	}
	if _, suspended := checkpoint.AsSuspension(err); suspended {
		return
	}
	llmTurn.Fail(err)
}
