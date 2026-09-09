/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package requeststatus

// Phase is the lifecycle state of a request that is still running or has
// stopped without a terminal [Outcome]. A string literal converts to Phase
// implicitly, so [Validator.Validate] rejects a misspelled value, not the
// compiler.
type Phase string

// Phase values. This vocabulary is fixed: it does not change when a Surface
// is rehosted or added.
const (
	// PhaseRunning means the bot is actively generating or regenerating the
	// change.
	PhaseRunning Phase = "running"
	// PhaseWaiting means the bot is waiting on an external signal, such as
	// checks or human review.
	PhaseWaiting Phase = "waiting"
	// PhaseNeedsYou means a person must change the request or make a
	// decision before the bot can continue.
	PhaseNeedsYou Phase = "needs_you"
	// PhaseFailed means the run stopped on an operational or unexpected
	// error.
	PhaseFailed Phase = "failed"
)

var validPhases = map[Phase]struct{}{
	PhaseRunning:  {},
	PhaseWaiting:  {},
	PhaseNeedsYou: {},
	PhaseFailed:   {},
}

// Activity refines [PhaseRunning] with the bot orchestrator's current step.
// It describes the bot, not the host, and survives every Surface cutover
// unchanged.
type Activity string

// Activity values.
const (
	// ActivityAnalyze is understanding the request before generating a
	// change.
	ActivityAnalyze Activity = "analyze"
	// ActivityEnrich is filling in the change's supporting detail.
	ActivityEnrich Activity = "enrich"
	// ActivityTestGen is generating the change's test coverage.
	ActivityTestGen Activity = "testgen"
	// ActivityValidate is checking the change before it is proposed.
	ActivityValidate Activity = "validate"
)

var validActivities = map[Activity]struct{}{
	ActivityAnalyze:  {},
	ActivityEnrich:   {},
	ActivityTestGen:  {},
	ActivityValidate: {},
}

// Outcome is a terminal result that is not a failure, mutually exclusive
// with an active [Phase].
type Outcome string

// Outcome values.
const (
	// OutcomeNoActionNeeded means the request was already satisfied and the
	// bot made no change.
	OutcomeNoActionNeeded Outcome = "no_action_needed"
	// OutcomeComplete means the change merged.
	OutcomeComplete Outcome = "complete"
	// OutcomeCanceled means a person stopped the request without merging
	// it.
	OutcomeCanceled Outcome = "canceled"
)

var validOutcomes = map[Outcome]struct{}{
	OutcomeNoActionNeeded: {},
	OutcomeComplete:       {},
	OutcomeCanceled:       {},
}

// Role names what a [Surface] is for — the requester's surface or the
// reviewer's surface — not what host it runs on. A [ChangeLink] never
// selects or implies a Role.
type Role string

// Role values.
const (
	// RoleRequest is the requester's and operator's surface, e.g. the
	// stereo issue.
	RoleRequest Role = "request"
	// RoleChange is the reviewer's surface, e.g. the stereo pull request.
	RoleChange Role = "change"
)

// ChangeLink names the proposed change a requester can follow: a pull
// request on GitHub, a merge request on GitLab. It is presentation only and
// never routes publication to a [Role] or a [Surface].
type ChangeLink struct {
	// Label is the link text, e.g. "PR #42".
	Label string
	// URL is the link target. Must be HTTPS on a host declared with
	// [WithLinkHosts].
	URL string
}
