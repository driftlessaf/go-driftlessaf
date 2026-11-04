/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// SuspendProvider constructs the declaration of the held-out suspend tool. It
// mirrors SubmitResultProvider so callers wire suspension the way they wire the
// terminal submit tool.
type SuspendProvider func() (*genai.FunctionDeclaration, error)

// WithSuspendTool registers the suspend tool advertised to the model. When the
// model calls it, Execute halts after the turn quiesces (every sibling call
// has produced its real function response) and returns a
// *checkpoint.Suspension: an error value carrying the envelope Resume needs.
//
// Without this option the suspend tool is never advertised and Execute behaves
// as before. The tool's name must differ from the submit tool's (checked in
// New) and from every caller-registered tool (checked in Execute, where the
// tool map is known).
func WithSuspendTool[Request promptbuilder.Bindable, Response any](provider SuspendProvider) Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		if provider == nil {
			return errors.New("suspend tool provider cannot be nil")
		}
		def, err := provider()
		if err != nil {
			return err
		}
		if def == nil || def.Name == "" {
			return errors.New("suspend tool definition must have a name")
		}
		e.suspendTool = def
		return nil
	}
}

// WithSuspendOnSubmit makes an accepted submission pause the conversation
// instead of ending it, for a caller that reviews the work and may send it
// back.
//
// When the submit tool commits a result, Execute returns that Response
// together with a *checkpoint.Suspension whose Reason is
// checkpoint.ReasonSubmitted and whose one pending call is the submit call.
// ResumeWithResults continues the conversation with the caller's review as
// that call's function response. Only the turn's first submission is
// reviewed; a later one gets an error result, and a suspend-tool call in the
// same turn is answered as not asked.
//
// The option requires a submit tool (WithSubmitResultProvider). It is part of
// the resume config digest, so an envelope parked with it does not resume on
// an executor without it.
func WithSuspendOnSubmit[Request promptbuilder.Bindable, Response any]() Option[Request, Response] {
	return func(e *executor[Request, Response]) error {
		e.suspendOnSubmit = true
		return nil
	}
}

// suspendToolName returns the suspend tool's name, or "" when WithSuspendTool
// was not applied.
func (e *executor[Request, Response]) suspendToolName() string {
	if e.suspendTool == nil {
		return ""
	}
	return e.suspendTool.Name
}

// providerState is the envelope's ProviderState for this executor.
type providerState struct {
	// History is the chat's curated history at the pause, verbatim. Its last
	// content is the model turn holding the pending call. Thinking models
	// reject a history whose thought signatures were altered, so it is
	// stored as the SDK encodes it and replayed unchanged.
	History []*genai.Content `json:"history"`
	// Responses are the paused turn's function responses for every call
	// except the pending ones, in call order. Resume sends them together
	// with the answers.
	Responses []*genai.Part `json:"responses,omitempty"`
}

// pendingCallID is the ID an envelope gives a pending call. Vertex AI often
// leaves FunctionCall.ID empty, and a pending call needs an ID to pair an
// answer to, so an empty one is derived from the call's name and its index
// among the model turn's function calls. Resume derives the same ID from the
// stored history.
func pendingCallID(call *genai.FunctionCall, index int) string {
	if call.ID != "" {
		return call.ID
	}
	return call.Name + "#" + strconv.Itoa(index)
}

// buildSuspension assembles the suspension returned when the model calls the
// suspend tool. history is the curated chat history, which must end with
// paused, the model content holding the pending call; slots holds that turn's
// function responses in call order with nil for the pending call. callIndex is
// the pending call's index among the turn's function calls. turnLimit is the
// turn at which the active run's budget runs out (startTurn+budget), so the
// envelope carries what is left of that budget rather than a figure recomputed
// from the live maxTurns, which a resume on a differently configured executor
// may not share.
func (e *executor[Request, Response]) buildSuspension(
	history []*genai.Content,
	paused *genai.Content,
	slots []*genai.Part,
	tools map[string]googletool.Metadata[Response],
	turn, turnLimit int,
	call *genai.FunctionCall,
	callIndex int,
	traceID string,
) (*checkpoint.Suspension, error) {
	// The SDK leaves a response out of the curated history when any of its
	// parts fails validateContent (a part carrying only a thought signature,
	// for one), and drops the input that produced it too. The history would
	// then end with an earlier model turn, and resume would pair the answers
	// against that turn's calls, so compare identity rather than role: the SDK
	// stores the candidate's content pointer itself.
	if len(history) == 0 || history[len(history)-1] != paused {
		return nil, errors.New("suspend: curated chat history does not end with the model turn holding the pending call")
	}
	responses := make([]*genai.Part, 0, len(slots))
	for _, p := range slots {
		if p != nil {
			responses = append(responses, p)
		}
	}
	state, err := json.Marshal(providerState{History: history, Responses: responses})
	if err != nil {
		return nil, fmt.Errorf("marshal provider state: %w", err)
	}
	input, err := json.Marshal(call.Args)
	if err != nil {
		return nil, fmt.Errorf("marshal pending call input: %w", err)
	}
	digest, err := e.configDigest(tools)
	if err != nil {
		return nil, err
	}
	return checkpoint.NewAskAFriendSuspension(checkpoint.ProviderGoogle, e.model, digest, turn, turnLimit,
		checkpoint.PendingToolCall{
			ID:        pendingCallID(call, callIndex),
			Name:      call.Name,
			InputJSON: input,
		}, state, nil, traceID), nil
}

// buildSubmitSuspension is buildSuspension for an accepted submission under
// WithSuspendOnSubmit.
func (e *executor[Request, Response]) buildSubmitSuspension(
	history []*genai.Content,
	paused *genai.Content,
	slots []*genai.Part,
	tools map[string]googletool.Metadata[Response],
	turn, turnLimit int,
	call *genai.FunctionCall,
	callIndex int,
	traceID string,
) (*checkpoint.Suspension, error) {
	susp, err := e.buildSuspension(history, paused, slots, tools, turn, turnLimit, call, callIndex, traceID)
	if err != nil {
		return nil, err
	}
	// The suspend-tool constructor derives a question from the call's input;
	// a submission carries none.
	susp.Reason, susp.Question = checkpoint.ReasonSubmitted, ""
	return susp, nil
}

// configDigest digests the turn-invariant request a resume must match to
// replay safely: generation settings, system instruction and tools. Resource
// labels are left out because they attribute billing and do not change what
// the model sees. The context cache name is never part of it, so a resume
// recreates the cache rather than depending on one the server may have
// expired.
//
// WithSuspendOnSubmit changes what a resumed submission does, so executors
// with it digest the flag too.
func (e *executor[Request, Response]) configDigest(tools map[string]googletool.Metadata[Response]) (string, error) {
	config, _, err := e.staticRequest(tools)
	if err != nil {
		return "", fmt.Errorf("build static request for digest: %w", err)
	}
	config.Labels = nil
	if e.suspendOnSubmit {
		return checkpoint.DigestJSON(struct {
			Config          *genai.GenerateContentConfig `json:"config"`
			SuspendOnSubmit bool                         `json:"suspend_on_submit"`
		}{Config: config, SuspendOnSubmit: true})
	}
	return checkpoint.DigestJSON(config)
}

// extraSubmissionPart answers a submission after the first in one turn under
// WithSuspendOnSubmit: only the first is reviewed.
func extraSubmissionPart(call *genai.FunctionCall) *genai.Part {
	return &genai.Part{FunctionResponse: googletool.Error(call,
		"only one submission per turn is reviewed; an earlier submit call in this turn is the one under review")}
}

// extraSuspendPart answers a suspend call after the first in one turn: one
// question is asked per pause.
func extraSuspendPart(call *genai.FunctionCall) *genai.Part {
	return &genai.Part{FunctionResponse: googletool.Error(call,
		"only one question per turn is asked; an earlier call in this turn is the one awaiting an answer")}
}

// notAskedPart answers a suspend call that a suspending submission in the same
// turn overtook. The question was never sent, and the model must know that,
// since its conversation continues.
func notAskedPart(call *genai.FunctionCall) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID:   call.ID,
		Name: call.Name,
		Response: map[string]any{
			"status": "not_asked",
			"reason": "you submitted in the same turn, so the question was not sent; ask again if you still need an answer",
		},
	}}
}
