/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor

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
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"
)

// Resumer resumes a conversation that suspended on the suspend tool (see
// WithSuspendTool). It is kept off Interface so existing implementers of
// Interface compile unchanged; a caller that parks and wakes runs
// type-asserts the executor New returns.
type Resumer[Request promptbuilder.Bindable, Response any] interface {
	// Resume rebuilds the paused conversation from env and continues it.
	// answers maps each pending call ID (env.PendingToolCalls[i].ID) to the
	// human's answer, framed by checkpoint.FramedAnswers; a missing entry is
	// framed as an explicit empty answer. It returns checkpoint.ErrConfigDrift
	// (wrapped) when the live executor configuration no longer matches the
	// envelope.
	Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, tools map[string]googletool.Metadata[Response]) (Response, error)
}

// ResultResumer resumes a suspended conversation with results the caller
// framed itself, for a pending call whose result is not a human's answer: a
// reviewer's verdict on a submission (WithSuspendOnSubmit), or the caller's
// own reply to a suspend call. Like Resumer it is kept off Interface.
type ResultResumer[Request promptbuilder.Bindable, Response any] interface {
	// ResumeWithResults is Resume with results, keyed by pending call ID,
	// used verbatim as the pending calls' function responses. Every pending
	// call needs a non-empty result.
	ResumeWithResults(ctx context.Context, env checkpoint.Envelope, results map[string]string, tools map[string]googletool.Metadata[Response]) (Response, error)
}

var (
	_ Interface[promptbuilder.Bindable, any]     = (*executor[promptbuilder.Bindable, any])(nil)
	_ Resumer[promptbuilder.Bindable, any]       = (*executor[promptbuilder.Bindable, any])(nil)
	_ ResultResumer[promptbuilder.Bindable, any] = (*executor[promptbuilder.Bindable, any])(nil)
)

// answerResponseKey is the function response field a pending call's answer is
// sent under: "output" is the key Gemini documents for a successful result.
const answerResponseKey = "output"

// Resume implements Resumer.
func (e *executor[Request, Response]) Resume(
	ctx context.Context,
	env checkpoint.Envelope,
	answers map[string]string,
	tools map[string]googletool.Metadata[Response],
) (Response, error) {
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
func (e *executor[Request, Response]) ResumeWithResults(
	ctx context.Context,
	env checkpoint.Envelope,
	results map[string]string,
	tools map[string]googletool.Metadata[Response],
) (Response, error) {
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
	tools map[string]googletool.Metadata[Response],
	answers func() (map[string]string, error),
) (response Response, err error) {
	var zero Response
	// Fail closed: a resume must rebuild the request against the same
	// backend, model and turn-invariant configuration that produced the
	// envelope, or the parked state is stale and the run must start over.
	digest, err := e.configDigest(tools)
	if err != nil {
		return zero, err
	}
	if err := checkpoint.ValidateForResume(env, checkpoint.ProviderGoogle, e.model, digest, time.Now()); err != nil {
		return zero, err
	}

	var state providerState
	if err := json.Unmarshal(env.ProviderState, &state); err != nil {
		return zero, fmt.Errorf("resume: unmarshal provider state: %w", err)
	}
	texts, err := answers()
	if err != nil {
		return zero, err
	}
	parts, err := pairResponses(state, texts)
	if err != nil {
		return zero, fmt.Errorf("resume: %w", err)
	}

	clog.InfoContext(ctx, "Resuming suspended Google AI agent execution",
		"key", env.ReconcilerKey, "run", env.RunID, "turn", env.Turn,
		"remaining_turns", env.RemainingTurns, "linked_trace", env.TraceID)

	trace, done := agenttrace.StartTrace[Response](ctx, fmt.Sprintf("resume suspended agent (key=%q, run=%q, reason=%q)", env.ReconcilerKey, env.RunID, env.Reason))
	defer func() {
		done(response, err)
	}()
	if env.TraceID != "" {
		if span := oteltrace.SpanFromContext(trace.Context()); span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String(agenttrace.AttrResumeLinkedTraceID, env.TraceID))
		}
	}

	// The envelope never names a context cache: the server expires caches on
	// their TTL, and a parked run can outlive one. prepareRun reuses this
	// executor's cache while it is valid and otherwise creates a new one.
	setup, err := e.prepareRun(ctx, tools)
	if err != nil {
		return zero, err
	}
	state.History = append(state.History, &genai.Content{Role: genai.RoleUser, Parts: parts})

	// The turn budget is loop configuration outside the digest, so the
	// envelope's remaining budget is capped at the live maxTurns.
	return e.runConversation(ctx, trace, setup, tools, state.History, nil, env.Turn+1, min(env.RemainingTurns, e.maxTurns))
}

// pairResponses builds the user input that answers the paused model turn: one
// function response per function call, in call order. A pending call gets its
// answer from texts; every other call takes the next stored response. The
// stored state must account for every call exactly once, or the restored
// request would leave a call unanswered, which the API rejects.
func pairResponses(state providerState, texts map[string]string) ([]*genai.Part, error) {
	if len(state.History) == 0 {
		return nil, errors.New("provider state carries no history")
	}
	paused := state.History[len(state.History)-1]
	if paused.Role != genai.RoleModel {
		return nil, fmt.Errorf("provider state ends with role %q, want %q", paused.Role, genai.RoleModel)
	}

	parts := make([]*genai.Part, 0, len(state.Responses)+len(texts))
	stored, answered, index := 0, 0, 0
	for _, p := range paused.Parts {
		call := p.FunctionCall
		if call == nil {
			continue
		}
		id := pendingCallID(call, index)
		index++
		if text, ok := texts[id]; ok {
			parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID:       call.ID,
				Name:     call.Name,
				Response: map[string]any{answerResponseKey: text},
			}})
			answered++
			continue
		}
		if stored >= len(state.Responses) {
			return nil, fmt.Errorf("function call %q has neither a stored response nor an answer", call.Name)
		}
		resp := state.Responses[stored]
		if resp.FunctionResponse == nil || resp.FunctionResponse.Name != call.Name {
			return nil, fmt.Errorf("stored response %d does not answer function call %q", stored, call.Name)
		}
		parts = append(parts, resp)
		stored++
	}
	if answered != len(texts) {
		return nil, fmt.Errorf("%d of %d pending calls are not in the paused model turn", len(texts)-answered, len(texts))
	}
	if stored != len(state.Responses) {
		return nil, fmt.Errorf("%d stored responses answer no function call", len(state.Responses)-stored)
	}
	return parts, nil
}
