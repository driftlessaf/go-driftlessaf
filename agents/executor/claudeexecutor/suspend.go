/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
)

// suspendProviderName is the checkpoint.Envelope.Provider value stamped by this
// executor. It names the executor backend that produced ProviderState (an
// anthropic.MessageNewParams JSON blob), NOT the serving provider (Vertex vs
// the first-party API) — a resume must reconstruct an Anthropic request
// regardless of which backend serves it.
const suspendProviderName = checkpoint.ProviderAnthropic

// SuspendProvider constructs the tool definition for the held-out ask-a-friend
// suspend tool. It mirrors SubmitResultProvider's shape (a constructor that can
// fail) so callers wire suspension the same way they wire the terminal submit
// tool.
type SuspendProvider func() (anthropic.ToolParam, error)

// WithSuspendTool registers the ask-a-friend suspend tool advertised to the model.
// When the model calls it, Execute halts after the turn quiesces (every sibling
// tool call has produced its real tool_result) and returns a
// *checkpoint.Suspension — an error value carrying the full envelope needed to
// resume later — instead of a Response.
//
// This is fully opt-in: without WithSuspendTool the suspend tool is never
// advertised, isSuspend is always false, and Execute's behavior is byte-for-byte
// unchanged. The suspend tool name must differ from the terminal submit tool's
// name (validated at construction) and from every caller-registered tool
// (validated in Execute, where the run's tool map is known); a collision would
// make dispatch ambiguous or silently drop the pause.
func WithSuspendTool[Request promptbuilder.Bindable, Response any](provider SuspendProvider) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if provider == nil {
			return errors.New("suspend tool provider cannot be nil")
		}
		def, err := provider()
		if err != nil {
			return err
		}
		if def.Name == "" {
			return errors.New("suspend tool definition must have a name")
		}
		e.suspendTool = &def
		return nil
	}
}

// suspendToolName returns the configured ask-a-friend suspend tool's name, or ""
// when WithSuspendTool was not applied. Symmetric with submitToolName.
func (e *executor[Request, Response]) suspendToolName() string {
	if e.suspendTool == nil {
		return ""
	}
	return e.suspendTool.Name
}

// buildSuspension assembles the checkpoint.Suspension returned when the model
// calls the suspend tool. params is the request as it stands at the pause point
// — the assistant tool_use message plus the siblings' real tool_results, with
// the suspend call left unanswered — captured verbatim as ProviderState so a
// resume can rebuild the exact Anthropic request. suspendCall is the tool_use
// block that triggered the pause; its provider-assigned ID is persisted so
// resume pairs the human answer back to this exact call rather than re-deriving
// it from the tool name.
func (e *executor[Request, Response]) buildSuspension(
	params anthropic.MessageNewParams,
	tools map[string]claudetool.Metadata[Response],
	turn int,
	suspendCall anthropic.ToolUseBlock,
	traceID string,
	state loopState,
) (*checkpoint.Suspension, error) {
	providerState, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal provider state: %w", err)
	}
	loopStateJSON, err := state.marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal loop state: %w", err)
	}

	// ConfigDigest is taken over the turn-invariant request prefix (tools,
	// system, sampling) — exactly the configuration a resume must match to
	// replay safely. buildStaticParams leaves Messages unset, so the digest is
	// stable across turns and independent of the growing conversation.
	staticParams, _, _, err := e.buildStaticParams(tools)
	if err != nil {
		return nil, fmt.Errorf("build static params for digest: %w", err)
	}
	digest, err := checkpoint.DigestJSON(staticParams)
	if err != nil {
		return nil, err
	}

	// NewAskAFriendSuspension stamps the schema version, the shared reason, and
	// the clamped remaining-turn budget; this executor supplies only the
	// provider-typed pieces. The parked turn index lives in Envelope.Turn and
	// Resume re-enters the loop at Turn+1; LoopState carries the per-run
	// counters that must survive the pause.
	return checkpoint.NewAskAFriendSuspension(suspendProviderName, e.modelName, digest, turn, e.maxTurns,
		checkpoint.PendingToolCall{
			ID:        suspendCall.ID,
			Name:      suspendCall.Name,
			InputJSON: normalizeToolUseInput(suspendCall.Input),
		}, providerState, loopStateJSON, traceID), nil
}

// loopState is the executor-loop bookkeeping a suspension carries so a resumed
// conversation continues the run's per-run bounds instead of restarting them.
// The turn index is not here: the envelope carries it as Envelope.Turn.
type loopState struct {
	// TruncatedToolCalls is the run's count of turns the output cap cut off
	// mid tool call, bounded by truncatedToolCallRetries.
	TruncatedToolCalls int `json:"truncated_tool_calls,omitempty"`
}

// loopStateKey is the envelope key the executor's state sits under: the
// provider name the envelope already carries, so the executor has one name.
// Namespacing lets decodeLoopState tell the executor's own record from a
// foreign object a caller wrote into LoopState, which would otherwise decode
// to zero counts without a word.
const loopStateKey = suspendProviderName

// marshal encodes the state for the envelope, or nil when nothing is set, so
// a run that never tripped a bound seals the same envelope it always did.
func (s loopState) marshal() (json.RawMessage, error) {
	if s == (loopState{}) {
		return nil, nil
	}
	return json.Marshal(map[string]loopState{loopStateKey: s})
}

// decodeLoopState reads a resumed envelope's loop state. An envelope sealed
// before any state was carried has none, and resumes with zero counts. A
// non-empty object without the executor's key was written by someone else;
// it resumes with zero counts too, but says so, since the per-run bounds
// the state carries restart from that point.
func decodeLoopState(ctx context.Context, raw json.RawMessage) (loopState, error) {
	if len(raw) == 0 {
		return loopState{}, nil
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return loopState{}, fmt.Errorf("resume: unmarshal loop state: %w", err)
	}
	own, ok := wrapped[loopStateKey]
	if !ok {
		clog.WarnContext(ctx, "the envelope's loop state carries no record under the executor's key; a caller replaced it, and the run's per-run bounds restart from here",
			"keys", slices.Sorted(maps.Keys(wrapped)))
		return loopState{}, nil
	}
	var s loopState
	if err := json.Unmarshal(own, &s); err != nil {
		return s, fmt.Errorf("resume: unmarshal loop state: %w", err)
	}
	return s, nil
}
