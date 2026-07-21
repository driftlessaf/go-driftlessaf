/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// Resumer is the capability interface a resume-capable meta-agent satisfies in
// addition to Agent. It mirrors Agent.Execute but continues a previously
// suspended conversation from a checkpoint.Envelope instead of starting fresh.
//
// Resumer is deliberately NOT folded into Agent: keeping it a separate,
// opt-in capability means the Agent interface never grows and every existing
// wrapper (usage-capturing decorators, adapters, downstream reconcilers, …)
// compiles unmodified. A caller that parks and wakes runs obtains it via
// AsResumer; a caller that only runs fresh conversations depends on Agent
// unchanged. This mirrors claudeexecutor.Resumer, which is likewise kept off
// the executor's exported Interface.
//
// Today only the Claude backend implements Resumer; the Gemini and
// OpenAI-compatible backends gain it together with suspend support in their
// executors (DEV-2247 follow-up slices), and until then AsResumer reports
// false for agents built on them.
//
// The answers map keys each pending tool-call ID (from
// Envelope.PendingToolCalls[i].ID — never re-derived from the tool name) to the
// human's answer. Answers flow raw: the underlying executor's Resume owns
// framing (checkpoint.FramedAnswers), and a missing entry is framed as an
// explicit empty-answer placeholder so no pending tool call is left
// unanswered. Resume returns checkpoint.ErrConfigDrift (wrapped) when the live
// executor config no longer matches the envelope, signaling the caller to
// rebuild from scratch.
type Resumer[Req promptbuilder.Bindable, Resp, CB any] interface {
	Agent[Req, Resp, CB]

	// Resume rebuilds the paused conversation from env and continues it with the
	// given tool callbacks and human answers.
	Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, callbacks CB) (Resp, error)
}

// AsResumer type-asserts agent to the Resumer capability. It reports false when
// the agent's backend does not support suspend/resume, so callers can branch to
// a fresh-run fallback rather than assume every backend can wake a checkpoint.
//
// Because a meta-agent builds its executor exactly once at construction, a
// resume path that must run against a *fresh* executor per wake should construct
// a new agent (via New) inside the reconcile and AsResumer that, rather than
// reuse a long-lived agent captured in main.
func AsResumer[Req promptbuilder.Bindable, Resp, CB any](agent Agent[Req, Resp, CB]) (Resumer[Req, Resp, CB], bool) {
	r, ok := agent.(Resumer[Req, Resp, CB])
	return r, ok
}
