/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/openai/openai-go/v3/responses"
)

// providerState is the envelope's ProviderState for this executor.
type providerState struct {
	// Input is the request input at the pause, one item per entry, each as it
	// was encoded on the wire: the prompt, every earlier turn, and last the
	// paused turn's output items. Requests use store: false, so these items,
	// encrypted reasoning included, are the only copy of the conversation and
	// are replayed byte for byte.
	Input []json.RawMessage `json:"input"`
	// Outputs are the paused turn's function_call_output items for every call
	// except the pending one, in call order. Resume sends them with the
	// answer.
	Outputs []json.RawMessage `json:"outputs,omitempty"`
}

// suspendToolName returns the suspend tool's name, or "" when none is
// configured.
func (e *executor[Request, Response]) suspendToolName() string {
	if e.config.SuspendTool == nil {
		return ""
	}
	return e.config.SuspendTool.Name
}

// buildSuspension assembles the suspension for a turn whose dispatch result
// names a pending call. input is the request input, which must end with the
// turn's output items; calls are the turn's function calls. turnLimit is the
// turn at which the active run's budget runs out, so the envelope carries
// what is left of that budget rather than a figure recomputed from the live
// MaxTurns, which a resume on a differently configured executor may not
// share.
func (e *executor[Request, Response]) buildSuspension(
	input responses.ResponseInputParam,
	calls []responses.ResponseFunctionToolCall,
	result dispatchResult,
	tools map[string]toolcall.Tool[Response],
	turn, turnLimit int,
	traceID string,
) (*checkpoint.Suspension, error) {
	state := providerState{Input: make([]json.RawMessage, 0, len(input))}
	for _, item := range input {
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("suspend: encode input item: %w", err)
		}
		state.Input = append(state.Input, b)
	}
	// Resume pairs answers against the trailing turn's calls, so that turn
	// must be the one that paused.
	paused, err := pausedCallIDs(state.Input)
	if err != nil {
		return nil, fmt.Errorf("suspend: %w", err)
	}
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		ids = append(ids, c.CallID)
	}
	if !slices.Equal(paused, ids) {
		return nil, errors.New("suspend: the saved input does not end with the paused turn's calls")
	}
	for i, item := range result.outputs {
		if i == result.pending {
			continue
		}
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("suspend: encode tool output: %w", err)
		}
		state.Outputs = append(state.Outputs, b)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal provider state: %w", err)
	}
	params, err := e.staticParams(tools)
	if err != nil {
		return nil, err
	}
	digest, err := e.configDigest(params)
	if err != nil {
		return nil, err
	}
	call := calls[result.pending]
	susp := checkpoint.NewAskAFriendSuspension(checkpoint.ProviderOpenAI, e.config.Model, digest, turn, turnLimit,
		checkpoint.PendingToolCall{ID: call.CallID, Name: call.Name, InputJSON: json.RawMessage(call.Arguments)},
		encoded, nil, traceID)
	if result.reason == checkpoint.ReasonSubmitted {
		// The suspend-tool constructor derives a question from the call's
		// input; a submission carries none.
		susp.Reason, susp.Question = checkpoint.ReasonSubmitted, ""
	}
	return susp, nil
}

// configDigest digests what a resume must match to replay safely: the
// turn-invariant request (model, instructions, tools, output limit, effort,
// tool choice) and the serving provider, whose encrypted reasoning another
// provider cannot read. Resource labels and loop limits are left out.
// SuspendOnSubmit changes what a resumed submission does, so it is digested
// when set.
func (e *executor[Request, Response]) configDigest(params responses.ResponseNewParams) (string, error) {
	return checkpoint.DigestJSON(struct {
		Provider        string                      `json:"provider"`
		Request         responses.ResponseNewParams `json:"request"`
		SuspendOnSubmit bool                        `json:"suspend_on_submit,omitempty"`
	}{Provider: e.config.Attribution.ProviderName, Request: params, SuspendOnSubmit: e.config.SuspendOnSubmit})
}

// inputItem is the part of an encoded input item that locates turn
// boundaries and function calls.
type inputItem struct {
	Type   string `json:"type"`
	Role   string `json:"role"`
	CallID string `json:"call_id"`
}

// pausedCallIDs returns the call IDs of the trailing turn's function calls,
// in order. The trailing turn is the run of model output items after the last
// function_call_output or caller message.
func pausedCallIDs(input []json.RawMessage) ([]string, error) {
	var ids []string
	for _, raw := range slices.Backward(input) {
		var item inputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("decode input item: %w", err)
		}
		if item.Type == "function_call_output" || item.Role == "user" || item.Role == "system" || item.Role == "developer" {
			break
		}
		if item.Type == "function_call" {
			ids = append(ids, item.CallID)
		}
	}
	slices.Reverse(ids)
	if len(ids) == 0 {
		return nil, errors.New("the input does not end with a turn of function calls")
	}
	return ids, nil
}
