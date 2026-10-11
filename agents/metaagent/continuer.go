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

// Continuer is the capability interface a meta-agent satisfies when it can
// continue a conversation that spent its turn budget, instead of starting it
// over: the Continuation an exhausted run carries on its
// executor.MaxTurnsError. Like Resumer it is kept off Agent, and a caller
// obtains it via AsContinuer.
//
// The typical caller holds two agents built from one Config that differ only
// in MaxTurns. When the smaller one's run is exhausted, it continues that
// conversation on the larger one, whose budget counts the whole conversation
// (see claudeexecutor.Continuer). The callbacks should be the set the
// exhausted run had: the model has seen those tools' results, and a tool that
// keeps state across calls must keep it across the continuation too.
//
// Today only the Claude backend implements Continuer. The Gemini and
// OpenAI-compatible executors return a bare executor.ErrMaxTurns, which
// carries no continuation, so a caller falls back to a fresh run for them.
type Continuer[Req promptbuilder.Bindable, Resp, CB any] interface {
	Agent[Req, Resp, CB]

	// Continue picks the exhausted conversation in env up after its last
	// turn and runs it with the given tool callbacks. It returns
	// checkpoint.ErrConfigDrift (wrapped) when this agent's configuration no
	// longer matches the envelope, signaling the caller to run from scratch.
	Continue(ctx context.Context, env checkpoint.Envelope, callbacks CB) (Resp, error)
}

// AsContinuer type-asserts agent to the Continuer capability. It reports false
// when the agent's backend cannot continue a conversation, so callers can
// branch to a fresh run.
func AsContinuer[Req promptbuilder.Bindable, Resp, CB any](agent Agent[Req, Resp, CB]) (Continuer[Req, Resp, CB], bool) {
	c, ok := agent.(Continuer[Req, Resp, CB])
	return c, ok
}
