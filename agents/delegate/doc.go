/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package delegate gives an agent tools to run background sub-agents.
//
// A delegate is a fresh conversation that does one piece of work and ends by
// calling its terminal report tool. The agent that starts it carries on
// meanwhile. [Tools] returns four tools for the starting agent's
// conversation:
//
//   - delegate(title, prompt, minutes?) starts a delegate and returns its id
//     at once.
//   - delegates lists them: state, elapsed time, tool calls, trace id, and
//     turns once stopped.
//   - delegate_peek(id, last?) shows a delegate's latest steps, each tool
//     result cut to [Options.StepResultLimit], and its report once stopped.
//   - delegate_cancel(id) stops one and returns its final state.
//
// A [Pool] owns the delegates of one conversation. It bounds how many run at
// once and how long each runs. A delegate stops when it reports, when its
// time box runs out, when it is cancelled, or when the Pool's context is
// cancelled. One that stops without reporting leaves a partial report: why it
// stopped and the reasons it gave for its tool calls.
//
// The caller supplies what is particular to it through [Options]: the
// delegate's tools ([Options.Tools]), how its conversation runs
// ([Options.Execute], usually a metaagent built with the caller's model and
// system prompt), and what happens when it stops ([Options.OnStop]: deliver
// the report, keep the transcript, clean up what it left running).
// [Options.OnTrace] and [Pool.LiveTurns] let the caller account for the
// delegates' model usage.
//
// The delegate tools are not available to a delegate unless [Options.Tools]
// includes them, so delegates nest only if the caller asks for it.
package delegate
