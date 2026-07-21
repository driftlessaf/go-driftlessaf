/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package checkpoint

import (
	"encoding/json"
	"slices"
	"time"
)

// EnvelopeVersion is the current schema version stamped into new envelopes.
// Bump it when a backwards-incompatible field change lands so a Store can
// refuse or migrate stale objects.
const EnvelopeVersion = 1

// PendingToolCall records a tool call that was issued by the model but whose
// result has not yet been produced. Because suspension fires post-quiesce —
// every dispatched sibling handler in the turn has finished and its real
// result is already in ProviderState's transcript — the only calls that can be
// pending are the held-out ask-a-friend call(s) from the suspended turn.
// Executors MUST NOT park a dispatched tool's call as pending: on resume every
// pending call is paired with framed human-answer text (see FramedAnswers),
// which would fabricate a result for a tool that never ran. The ID is the
// provider's own tool_use / function-call identifier and MUST be persisted
// verbatim: resume pairs the answer tool_result back to this exact ID rather
// than re-deriving it from Name, which would break as soon as a turn issues
// two calls to one tool.
type PendingToolCall struct {
	// ID is the provider-assigned tool call identifier (anthropic tool_use id,
	// openai tool_call id, genai FunctionCall.ID). Persisted, never re-derived.
	ID string `json:"id"`
	// Name is the tool name, retained for diagnostics/telemetry only.
	Name string `json:"name"`
	// InputJSON is the raw tool input as the model emitted it.
	InputJSON json.RawMessage `json:"input_json,omitempty"`
}

// Envelope is the provider-neutral, serializable capture of a suspended agent
// conversation. It carries enough state to reconstruct the request on resume
// without holding a live process across the wait.
type Envelope struct {
	// Version is the envelope schema version (see EnvelopeVersion).
	Version int `json:"version"`

	// Provider identifies the executor backend ("anthropic", "openai",
	// "google") that produced ProviderState.
	Provider string `json:"provider"`
	// Model is the model id in effect when the conversation suspended.
	Model string `json:"model"`
	// SDKVersion records the provider SDK version in effect at park time, for
	// diagnostics and telemetry. It is not compared on wake: SDK drift is
	// enforced through ConfigDigest — executors include the SDK version in the
	// digested configuration so a drifted binary trips the one fail-closed
	// gate rather than a second parallel check.
	SDKVersion string `json:"sdk_version,omitempty"`
	// ConfigDigest is DigestJSON over the resume-relevant configuration,
	// including the provider SDK version. A mismatch on wake signals "rebuild
	// from scratch" rather than resume. The digest is required: Validate
	// rejects an envelope without one at park time and ValidateForResume
	// rejects an empty digest on either side, so the drift gate can never be
	// vacuously satisfied.
	ConfigDigest string `json:"config_digest,omitempty"`

	// ReconcilerKey is the workqueue key the suspended run belongs to; combined
	// with the executor identity it names the single parked object.
	ReconcilerKey string `json:"reconciler_key"`
	// RunID disambiguates successive runs of the same key. It is a field of the
	// single per-{identity}/{key} object, NOT part of the storage path, so
	// Load(key) is never ambiguous.
	RunID string `json:"run_id"`

	// Turn is the 0-based conversation turn at which the suspension fired.
	Turn int `json:"turn"`
	// RemainingTurns is the overall turn budget still available across resumes,
	// so a conversation cannot loop forever by suspending and waking. It must
	// be positive to park: Validate rejects an exhausted budget at suspend
	// time, mirroring the ValidateForResume turn gate, so an envelope that
	// could never wake is never persisted.
	RemainingTurns int `json:"remaining_turns"`

	// Reason is a short machine-readable reason for the suspension.
	Reason string `json:"reason,omitempty"`

	// PendingToolCalls are the unanswered held-out ask-a-friend calls from the
	// suspended turn (usually one; more only when the turn issued several).
	// Dispatched siblings never appear here — their real results were already
	// in ProviderState when the turn quiesced (see PendingToolCall).
	PendingToolCalls []PendingToolCall `json:"pending_tool_calls,omitempty"`

	// ProviderState is the full provider request payload (e.g. a serialized
	// anthropic.MessageNewParams) carried verbatim as raw JSON.
	//
	// The envelope's neutrality stops at this field: the SCHEMA is one format
	// for every executor, but the conversation inside ProviderState is bound
	// to the exact Provider and Model that produced it, and ValidateForResume
	// fails closed on any mismatch (including a model version bump). This is
	// deliberate — provider transcripts are not losslessly translatable:
	// Anthropic thinking blocks carry model-bound signatures, Gemini thinking
	// mode requires the thoughtSignature history replayed verbatim, and tool
	// call identifier schemes differ per provider. A cross-model "resume" that
	// silently transplanted a transcript would replay unverifiable state.
	//
	// TODO(DEV-2247): support continuing on a different model. One model's
	// conversation can never be handed to another model directly — it will not
	// verify or replay. The workable approach: start a brand-new run on the
	// target model and feed it the parked conversation as plain text, together
	// with the pending question and the friend's answer. Everything needed for
	// that already lives in ProviderState, so no envelope schema change is
	// required.
	ProviderState json.RawMessage `json:"provider_state,omitempty"`
	// LoopState is executor-loop bookkeeping (turn index, cache-tail state, ...)
	// carried opaquely so each executor owns its own shape.
	LoopState json.RawMessage `json:"loop_state,omitempty"`

	// StateRef optionally points at externally stored large state (e.g. a GCS
	// object) when the payload is too big to inline.
	StateRef string `json:"state_ref,omitempty"`
	// TraceID is the originating trace, so the resumed run can link back to it.
	TraceID string `json:"trace_id,omitempty"`

	// Deadline is the wall-clock time after which the envelope must not be
	// resumed — ValidateForResume rejects an envelope whose Deadline has
	// passed (fail-closed on wake). Zero means no deadline.
	Deadline time.Time `json:"deadline,omitempty"`
}

// Clone returns a deep copy of the envelope so a Store can hand back a value
// that is safe against later caller mutation (and vice versa). The raw-JSON and
// slice fields are copied; nil stays nil to preserve byte-for-byte equality.
func (e *Envelope) Clone() *Envelope {
	if e == nil {
		return nil
	}
	cp := *e
	if e.PendingToolCalls != nil {
		cp.PendingToolCalls = make([]PendingToolCall, len(e.PendingToolCalls))
		for i, p := range e.PendingToolCalls {
			cp.PendingToolCalls[i] = p
			cp.PendingToolCalls[i].InputJSON = slices.Clone(p.InputJSON)
		}
	}
	cp.ProviderState = slices.Clone(e.ProviderState)
	cp.LoopState = slices.Clone(e.LoopState)
	return &cp
}
