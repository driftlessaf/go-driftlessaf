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
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chainguard-dev/clog"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Resumer is the capability interface for resuming a conversation that
// previously suspended (see WithSuspendTool). It is deliberately NOT part of the
// exported Interface: a caller that only runs fresh conversations depends on
// Interface unchanged, while a caller that parks and wakes runs type-asserts to
// Resumer. Keeping resume off Interface is the load-bearing compatibility rule —
// every existing consumer (dfc, judge, metaagent wrappers) compiles unmodified.
//
// The Request type parameter is unused by Resume itself (the entire request is
// restored from the checkpoint envelope, not re-bound from a Request) but is
// retained so Resumer mirrors Interface's shape and a single concrete executor
// satisfies both.
type Resumer[Request promptbuilder.Bindable, Response any] interface {
	// Resume rebuilds the paused conversation from env and continues it. answers
	// maps each pending tool_use ID (env.PendingToolCalls[i].ID) to the human's
	// answer; a missing entry is framed as an explicit empty-answer placeholder
	// so no pending tool_use is left unanswered (which the API rejects with a
	// non-retryable 400). It returns checkpoint.ErrConfigDrift (wrapped) when the
	// live executor config no longer matches the envelope, signaling the caller
	// to rebuild from scratch rather than resume against stale state.
	Resume(ctx context.Context, env checkpoint.Envelope, answers map[string]string, tools map[string]claudetool.Metadata[Response]) (Response, error)
}

// ResultResumer resumes a suspended conversation with results the caller
// framed itself, for a pending call whose result is not a human's answer: a
// reviewer's verdict on a submission that suspended
// (WithSuspendOnSubmit), or the caller's refusal of a suspend call. Resume
// frames every answer as a human's; ResumeWithResults places each result in
// the transcript as it is. Like Resumer it is kept off Interface and reached
// by type assertion.
type ResultResumer[Request promptbuilder.Bindable, Response any] interface {
	// ResumeWithResults is Resume with results, keyed by pending tool_use
	// ID, used verbatim as the pending calls' tool results. Every pending
	// call needs a non-empty result: there is no placeholder to fall back
	// on, and an empty tool result is a non-retryable 400.
	ResumeWithResults(ctx context.Context, env checkpoint.Envelope, results map[string]string, tools map[string]claudetool.Metadata[Response]) (Response, error)
}

// The concrete executor satisfies Interface (fresh runs), Resumer and
// ResultResumer (resumed runs); a resume-capable caller obtains the latter by
// type-asserting the Interface returned by New.
var (
	_ Interface[promptbuilder.Bindable, any]     = (*executor[promptbuilder.Bindable, any])(nil)
	_ Resumer[promptbuilder.Bindable, any]       = (*executor[promptbuilder.Bindable, any])(nil)
	_ ResultResumer[promptbuilder.Bindable, any] = (*executor[promptbuilder.Bindable, any])(nil)
)

// Resume implements Resumer. It reconstructs the Anthropic request from the
// checkpoint envelope (restoreParams), pairs the framed human answer to the
// persisted pending tool_use ID, and drives the shared turn loop for the
// envelope's remaining turn budget under a new trace linked back to the
// originating one (runRestored).
func (e *executor[Request, Response]) Resume(
	ctx context.Context,
	env checkpoint.Envelope,
	answers map[string]string,
	tools map[string]claudetool.Metadata[Response],
) (Response, error) {
	return e.resume(ctx, env, tools, func(params *anthropic.MessageNewParams) error {
		return appendPendingAnswers(params, env.PendingToolCalls, answers)
	})
}

// ResumeWithResults implements ResultResumer.
func (e *executor[Request, Response]) ResumeWithResults(
	ctx context.Context,
	env checkpoint.Envelope,
	results map[string]string,
	tools map[string]claudetool.Metadata[Response],
) (Response, error) {
	return e.resume(ctx, env, tools, func(params *anthropic.MessageNewParams) error {
		return appendPendingResults(params, env.PendingToolCalls, results)
	})
}

// resume is Resume and ResumeWithResults: pair fills the pending calls' slot
// in the restored request.
func (e *executor[Request, Response]) resume(
	ctx context.Context,
	env checkpoint.Envelope,
	tools map[string]claudetool.Metadata[Response],
	pair func(*anthropic.MessageNewParams) error,
) (Response, error) {
	var zero Response
	// Fail-closed configuration checks. A resume must reconstruct the request
	// against the same backend, model, and turn-invariant prefix that produced
	// the envelope; any drift means the parked state is stale and the run must be
	// rebuilt from scratch rather than replayed. ValidateForResume surfaces the
	// mismatches as checkpoint.ErrConfigDrift so a waker can branch on errors.Is,
	// and also enforces the remaining-turn budget and any envelope deadline before
	// any state is touched. ValidateForResume takes now as a parameter (rather
	// than reading the clock itself) so the checkpoint package's own deadline
	// tests can inject one; this call site supplies wall-clock time.
	digest, err := e.configDigest(tools)
	if err != nil {
		return zero, err
	}
	if err := checkpoint.ValidateForResume(env, suspendProviderName, e.modelName, digest, time.Now()); err != nil {
		return zero, err
	}

	params, err := e.restoreParams(env, tools)
	if err != nil {
		return zero, fmt.Errorf("resume: %w", err)
	}

	// Pair the framed human answer to the persisted pending tool_use ID(s). This
	// completes the tool_use/tool_result pairing the API requires: the suspend
	// call was left unanswered in the captured transcript so the answer could
	// take its slot on resume.
	if err := pair(&params); err != nil {
		return zero, err
	}
	state, err := decodeLoopState(ctx, env.LoopState)
	if err != nil {
		return zero, err
	}

	clog.InfoContext(ctx, "Resuming suspended Claude agent execution",
		"key", env.ReconcilerKey, "run", env.RunID, "turn", env.Turn,
		"remaining_turns", env.RemainingTurns, "linked_trace", env.TraceID)

	// Cap the envelope's remaining budget at the live executor's configured
	// maxTurns. The turn budget is loop config, not part of the request digest,
	// so ValidateForResume cannot catch a mismatch: without the cap, an envelope
	// parked under a larger budget (an operator lowering maxTurns between park
	// and wake, or a tampered checkpoint) would resume with more turns than the
	// live configuration allows.
	return e.runRestored(ctx, env, params, tools, state, min(env.RemainingTurns, e.maxTurns), resumeTracePrompt(env))
}

// restoreParams rebuilds the Anthropic request an envelope captured, ready for
// the turn loop to send again. It is the part Resume and Continue share.
func (e *executor[Request, Response]) restoreParams(env checkpoint.Envelope, tools map[string]claudetool.Metadata[Response]) (anthropic.MessageNewParams, error) {
	// Restore the provider request exactly as captured.
	var params anthropic.MessageNewParams
	if err := json.Unmarshal(env.ProviderState, &params); err != nil {
		return params, fmt.Errorf("unmarshal provider state: %w", err)
	}

	// Strip every message-block cache_control marker BEFORE the tail tracker
	// runs. The captured transcript carries the capture-time tail markers; left
	// in place, tail.advance would add fresh markers on top of them and the
	// request would exceed the four-breakpoint API limit (a non-retryable 400).
	// The static-prefix markers (tool definitions, system) are left intact —
	// they are re-counted by newTailBreakpoints so the reseeded tail stays
	// within budget.
	stripMessageCacheControl(params.Messages)

	// Re-stamp the static prefix from the live configuration. The digest
	// excludes the cache TTL, so an envelope captured under one TTL validates
	// under another, but the captured tool and system markers still carry the
	// capture-time TTL: left in place, a 5-minute marker would precede the
	// live 1-hour tail markers, which the API rejects. The digest has proved
	// the rest of the prefix identical, so the restored request matches what
	// Execute would build.
	live, _, _, err := e.buildStaticParams(tools, e.cacheTTL)
	if err != nil {
		return params, fmt.Errorf("build static params: %w", err)
	}
	params.Tools, params.System = live.Tools, live.System
	return params, nil
}

// runRestored drives the shared turn loop over a restored request for budget
// turns, starting after the envelope's turn. The run gets a NEW trace, linked
// back to the one the envelope came from, so the two halves of the
// conversation join downstream.
func (e *executor[Request, Response]) runRestored(
	ctx context.Context,
	env checkpoint.Envelope,
	params anthropic.MessageNewParams,
	tools map[string]claudetool.Metadata[Response],
	state loopState,
	budget int,
	tracePrompt string,
) (response Response, err error) {
	trace, done := agenttrace.StartTrace[Response](ctx, tracePrompt)
	defer func() {
		done(response, err)
	}()
	if env.TraceID != "" {
		if span := oteltrace.SpanFromContext(trace.Context()); span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String(agenttrace.AttrResumeLinkedTraceID, env.TraceID))
		}
	}

	// A fresh tail tracker: its budget is derived from the (surviving) static
	// prefix markers, and it reseeds markers on the growing tail as the
	// conversation goes on.
	tail := newTailBreakpoints(params, e.cacheTTL)
	return e.runConversation(ctx, trace, params, tools, tail, env.Turn+1, budget, nil, state)
}

// stripMessageCacheControl clears the cache_control marker on every content
// block of every message, routing through the same blockCacheControl accessor
// the tail tracker uses so support and clearing can never diverge. Blocks whose
// variant cannot carry a marker (thinking blocks) are skipped.
func stripMessageCacheControl(messages []anthropic.MessageParam) {
	for m := range messages {
		for b := range messages[m].Content {
			if cc := blockCacheControl(&messages[m].Content[b]); cc != nil {
				// The SDK tags CacheControl omitzero, so the zero value marshals
				// as no marker.
				*cc = anthropic.CacheControlEphemeralParam{}
			}
		}
	}
}

// appendPendingAnswers appends one framed tool_result per pending tool call to
// the final (user) message of the restored transcript, pairing each to its
// persisted tool_use ID. The pending tool_use blocks live in the last assistant
// turn, so their results must go in the user message immediately following it —
// the last message of the captured state. Framing (delimiters, the
// DefaultAnswerMaxBytes cap, and the explicit empty-answer placeholder for a
// call with no supplied answer) is checkpoint.FramedAnswers' job; this function
// only maps the framed answers into the Anthropic tool_result shape.
func appendPendingAnswers(params *anthropic.MessageNewParams, pending []checkpoint.PendingToolCall, answers map[string]string) error {
	last, err := lastUserMessage(params)
	if err != nil {
		return fmt.Errorf("resume: %w (the slot the answer pairs into)", err)
	}
	framed, err := checkpoint.FramedAnswers(pending, answers, 0)
	if err != nil {
		return err
	}
	appendToolResults(last, framed)
	return nil
}

// appendToolResults appends one text tool_result per answer to msg.
func appendToolResults(msg *anthropic.MessageParam, answers []checkpoint.FramedAnswer) {
	for _, a := range answers {
		msg.Content = append(msg.Content, anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: a.ID,
				Content: []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{Text: a.Text},
				}},
			},
		})
	}
}

// lastUserMessage returns the restored transcript's final message, which must
// be a user turn: it is where a resume pairs its answers, and what a continued
// conversation sends next.
func lastUserMessage(params *anthropic.MessageNewParams) (*anthropic.MessageParam, error) {
	if len(params.Messages) == 0 {
		return nil, errors.New("provider state carries no messages")
	}
	last := &params.Messages[len(params.Messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		return nil, fmt.Errorf("last message role %q, want user", last.Role)
	}
	return last, nil
}

// resumeTracePrompt is the prompt string recorded on a resumed run's trace. The
// restored transcript is the real context; this string only labels the resume
// so the trace is self-describing.
func resumeTracePrompt(env checkpoint.Envelope) string {
	return fmt.Sprintf("resume suspended agent (key=%q, run=%q, reason=%q)", env.ReconcilerKey, env.RunID, env.Reason)
}

// appendPendingResults pairs each pending call with the caller's result,
// verbatim, in the restored request's last user message.
func appendPendingResults(params *anthropic.MessageNewParams, pending []checkpoint.PendingToolCall, results map[string]string) error {
	if len(pending) == 0 {
		return errors.New("resume: the envelope has no pending tool calls to answer")
	}
	last, err := lastUserMessage(params)
	if err != nil {
		return fmt.Errorf("resume: %w (the slot the result pairs into)", err)
	}
	verbatim := make([]checkpoint.FramedAnswer, 0, len(pending))
	for _, pc := range pending {
		if pc.ID == "" {
			return errors.New("resume: pending tool call has an empty ID; cannot pair a result")
		}
		text := results[pc.ID]
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("resume: no result for pending call %q (%s)", pc.ID, pc.Name)
		}
		verbatim = append(verbatim, checkpoint.FramedAnswer{ID: pc.ID, Name: pc.Name, Text: text})
	}
	appendToolResults(last, verbatim)
	return nil
}
