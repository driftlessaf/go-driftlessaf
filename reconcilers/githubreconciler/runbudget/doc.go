/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package runbudget bounds one reconcile in two phases so that a run ends on
// the bot's terms rather than the platform's.
//
// A reconcile that drives an agent and then publishes its work (commit, push,
// pull request create or update, comments) runs under a platform request cap.
// Left alone, the cap arrives as a plain cancellation that discards the whole
// run, including work the agent had already finished, and the retry starts
// from scratch. A Budget splits the run instead: the agent phase ends at
// start + Total - Reserve, and the publication phase, which runs detached from
// the request's cancellation, ends at start + Total. A bot sizes Total below
// the platform cap.
//
// Start records a budget on the request context; AgentContext and
// PublicationContext derive the two phase contexts anywhere below it; Classify
// tells a caller whether the agent finished on its own, ran into the budget's
// deadline, or was interrupted by the request ending first (the platform may
// end a request before the budget, for example during a rollout). Without a
// recorded budget every function is a no-op, so bots that do not opt in keep
// their unbounded behavior.
package runbudget
