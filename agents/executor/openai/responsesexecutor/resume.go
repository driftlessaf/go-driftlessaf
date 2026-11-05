/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Resumer resumes a conversation that suspended on Config.SuspendTool. It is
// kept off Interface so existing implementers of Interface compile unchanged;
// a caller that parks and wakes runs type-asserts the executor New returns.
type Resumer[Request promptbuilder.Bindable, Response any] interface {
	// Resume rebuilds the paused conversation from env and continues it.
	// answers maps each pending call ID (env.PendingToolCalls[i].ID) to the
	// human's answer, framed by checkpoint.FramedAnswers; a missing entry is
	// framed as an explicit empty answer. It returns checkpoint.ErrConfigDrift
	// (wrapped) when the live executor configuration no longer matches the
	// envelope.
	Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, tools map[string]toolcall.Tool[Response]) (Response, error)
}

// ResultResumer resumes a suspended conversation with results the caller
// framed itself, for a pending call whose result is not a human's answer: a
// reviewer's verdict on a submission (Config.SuspendOnSubmit), or the
// caller's own reply to a suspend call. Like Resumer it is kept off
// Interface.
type ResultResumer[Request promptbuilder.Bindable, Response any] interface {
	// ResumeWithResults is Resume with results, keyed by pending call ID,
	// sent verbatim as the pending calls' function_call_output. Every pending
	// call needs a non-empty result.
	ResumeWithResults(ctx context.Context, env checkpoint.Envelope, results map[string]string, tools map[string]toolcall.Tool[Response]) (Response, error)
}

var (
	_ Interface[promptbuilder.Bindable, any]     = (*executor[promptbuilder.Bindable, any])(nil)
	_ Resumer[promptbuilder.Bindable, any]       = (*executor[promptbuilder.Bindable, any])(nil)
	_ ResultResumer[promptbuilder.Bindable, any] = (*executor[promptbuilder.Bindable, any])(nil)
)

// Resume implements Resumer.
func (e *executor[Request, Response]) Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, tools map[string]toolcall.Tool[Response]) (Response, error) {
	return e.resume(ctx, env, tools, func() (map[string]string, error) {
		framed, err := checkpoint.FramedAnswers(env.PendingToolCalls, answers, 0)
		if err != nil {
			return nil, err
		}
		texts := make(map[string]string, len(framed))
		for _, a := range framed {
			texts[a.ID] = a.Text
		}
		return texts, nil
	})
}

// ResumeWithResults implements ResultResumer.
func (e *executor[Request, Response]) ResumeWithResults(ctx context.Context, env checkpoint.Envelope, results map[string]string, tools map[string]toolcall.Tool[Response]) (Response, error) {
	return e.resume(ctx, env, tools, func() (map[string]string, error) {
		if len(env.PendingToolCalls) == 0 {
			return nil, errors.New("resume: the envelope has no pending tool calls to answer")
		}
		texts := make(map[string]string, len(env.PendingToolCalls))
		for _, pc := range env.PendingToolCalls {
			if pc.ID == "" {
				return nil, errors.New("resume: pending tool call has an empty ID; cannot pair a result")
			}
			text := results[pc.ID]
			if strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("resume: no result for pending call %q (%s)", pc.ID, pc.Name)
			}
			texts[pc.ID] = text
		}
		return texts, nil
	})
}

// resume is Resume and ResumeWithResults. answers returns the text sent for
// each pending call, keyed by its envelope ID.
func (e *executor[Request, Response]) resume(
	ctx context.Context,
	env checkpoint.Envelope,
	tools map[string]toolcall.Tool[Response],
	answers func() (map[string]string, error),
) (response Response, err error) {
	ctx, cancel := context.WithTimeout(ctx, e.config.ExecutionTimeout)
	defer cancel()
	// Fail closed: a resume must rebuild the request against the same
	// provider, model and turn-invariant configuration that produced the
	// envelope, or the parked state is stale and the run must start over.
	params, err := e.staticParams(tools)
	if err != nil {
		return response, err
	}
	digest, err := e.configDigest(params)
	if err != nil {
		return response, err
	}
	if err := checkpoint.ValidateForResume(env, checkpoint.ProviderOpenAI, e.config.Model, digest, time.Now()); err != nil {
		return response, err
	}
	var state providerState
	if err := json.Unmarshal(env.ProviderState, &state); err != nil {
		return response, fmt.Errorf("resume: unmarshal provider state: %w", err)
	}
	texts, err := answers()
	if err != nil {
		return response, err
	}
	input, seen, err := restoreInput(state, texts)
	if err != nil {
		return response, fmt.Errorf("resume: %w", err)
	}

	clog.InfoContext(ctx, "Resuming suspended Responses agent execution",
		"key", env.ReconcilerKey, "run", env.RunID, "turn", env.Turn,
		"remaining_turns", env.RemainingTurns, "linked_trace", env.TraceID)
	trace, done := agenttrace.StartTrace[Response](ctx, fmt.Sprintf("resume suspended agent (key=%q, run=%q, reason=%q)", env.ReconcilerKey, env.RunID, env.Reason))
	defer func() { done(response, err) }()
	if env.TraceID != "" {
		if span := oteltrace.SpanFromContext(trace.Context()); span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String(agenttrace.AttrResumeLinkedTraceID, env.TraceID))
		}
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	// The turn budget is loop configuration outside the digest, so the
	// envelope's remaining budget is capped at the live MaxTurns.
	return e.runConversation(ctx, trace, params, tools, seen, env.Turn+1, min(env.RemainingTurns, e.config.MaxTurns))
}

// restoreInput rebuilds the request input that answers the paused turn: the
// saved items verbatim, then one function_call_output per paused call in call
// order. A pending call gets its text from texts; every other call takes the
// next saved output. The state must account for every call exactly once, or
// the request would leave a call unanswered, which the API rejects. It also
// returns every function call ID in the conversation, which the turn loop
// refuses to see repeated.
func restoreInput(state providerState, texts map[string]string) (responses.ResponseInputParam, map[string]struct{}, error) {
	paused, err := pausedCallIDs(state.Input)
	if err != nil {
		return nil, nil, fmt.Errorf("provider state: %w", err)
	}
	input := make(responses.ResponseInputParam, 0, len(state.Input)+len(paused))
	seen := make(map[string]struct{})
	for _, raw := range state.Input {
		var item inputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, fmt.Errorf("decode input item: %w", err)
		}
		if item.Type == "function_call" {
			seen[item.CallID] = struct{}{}
		}
		// Override sends the saved bytes as they are, so encrypted reasoning
		// is never decoded and re-encoded.
		input = append(input, param.Override[responses.ResponseInputItemUnionParam](raw))
	}
	stored, answered := 0, 0
	for _, id := range paused {
		if text, ok := texts[id]; ok {
			if len(text) > maxPayloadBytes {
				return nil, nil, fmt.Errorf("the result for call %q exceeds the byte limit", id)
			}
			input = append(input, functionCallOutput(id, text))
			answered++
			continue
		}
		if stored >= len(state.Outputs) {
			return nil, nil, fmt.Errorf("function call %q has neither a saved output nor an answer", id)
		}
		raw := state.Outputs[stored]
		var item inputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, fmt.Errorf("decode saved output: %w", err)
		}
		if item.Type != "function_call_output" || item.CallID != id {
			return nil, nil, fmt.Errorf("saved output %d does not answer function call %q", stored, id)
		}
		input = append(input, param.Override[responses.ResponseInputItemUnionParam](raw))
		stored++
	}
	if answered != len(texts) {
		return nil, nil, fmt.Errorf("%d of %d pending calls are not in the paused turn", len(texts)-answered, len(texts))
	}
	if stored != len(state.Outputs) {
		return nil, nil, fmt.Errorf("%d saved outputs answer no function call", len(state.Outputs)-stored)
	}
	return input, seen, nil
}
