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

// The concrete executor satisfies both Interface (fresh runs) and Resumer
// (resumed runs); a resume-capable caller obtains the latter by type-asserting
// the Interface returned by New.
var (
	_ Interface[promptbuilder.Bindable, any] = (*executor[promptbuilder.Bindable, any])(nil)
	_ Resumer[promptbuilder.Bindable, any]   = (*executor[promptbuilder.Bindable, any])(nil)
)

// Resume implements Resumer. It reconstructs the Anthropic request from the
// checkpoint envelope, strips every stale message-block cache_control marker so
// the moving tail tracker can reseed fresh markers within the API's
// four-breakpoint limit (a verbatim replay would carry the suspend-time markers
// AND the reseeded ones, exceeding the limit and drawing a non-retryable 400),
// pairs the framed human answer to the persisted pending tool_use ID, and drives
// the shared turn loop for the envelope's remaining turn budget under a new trace
// linked back to the originating one.
func (e *executor[Request, Response]) Resume(
	ctx context.Context,
	env checkpoint.Envelope,
	answers map[string]string,
	tools map[string]claudetool.Metadata[Response],
) (response Response, err error) {
	// Fail-closed configuration checks. A resume must reconstruct the request
	// against the same backend, model, and turn-invariant prefix that produced
	// the envelope; any drift means the parked state is stale and the run must be
	// rebuilt from scratch rather than replayed. ValidateForResume surfaces the
	// mismatches as checkpoint.ErrConfigDrift so a waker can branch on errors.Is,
	// and also enforces the remaining-turn budget and any envelope deadline before
	// any state is touched. ValidateForResume takes now as a parameter (rather
	// than reading the clock itself) so the checkpoint package's own deadline
	// tests can inject one; this call site supplies wall-clock time.
	staticParams, _, _, serr := e.buildStaticParams(tools)
	if serr != nil {
		return response, fmt.Errorf("build static params for digest: %w", serr)
	}
	digest, derr := checkpoint.DigestJSON(staticParams)
	if derr != nil {
		return response, derr
	}
	if verr := checkpoint.ValidateForResume(env, suspendProviderName, e.modelName, digest, time.Now()); verr != nil {
		return response, verr
	}

	// Restore the provider request exactly as captured at the suspension point.
	var params anthropic.MessageNewParams
	if uerr := json.Unmarshal(env.ProviderState, &params); uerr != nil {
		return response, fmt.Errorf("resume: unmarshal provider state: %w", uerr)
	}

	// Strip every message-block cache_control marker BEFORE the tail tracker
	// runs. The captured transcript carries the suspend-time tail markers; left
	// in place, tail.advance would add fresh markers on top of them and the
	// request would exceed the four-breakpoint API limit. The static-prefix
	// markers (tool definitions, system) are left intact — they are re-counted by
	// newTailBreakpoints so the reseeded tail stays within budget.
	stripMessageCacheControl(params.Messages)

	// Pair the framed human answer to the persisted pending tool_use ID(s). This
	// completes the tool_use/tool_result pairing the API requires: the suspend
	// call was left unanswered in the captured transcript so the answer could
	// take its slot on resume.
	if aerr := appendPendingAnswers(&params, env.PendingToolCalls, answers); aerr != nil {
		return response, aerr
	}

	// Start a NEW trace for the resumed run, linked back to the originating trace
	// so the two halves of a paused conversation join downstream.
	trace, done := agenttrace.StartTrace[Response](ctx, resumeTracePrompt(env))
	defer func() {
		done(response, err)
	}()
	if env.TraceID != "" {
		if span := oteltrace.SpanFromContext(trace.Context()); span.SpanContext().IsValid() {
			span.SetAttributes(attribute.String(agenttrace.AttrResumeLinkedTraceID, env.TraceID))
		}
	}

	clog.InfoContext(ctx, "Resuming suspended Claude agent execution",
		"key", env.ReconcilerKey, "run", env.RunID, "turn", env.Turn,
		"remaining_turns", env.RemainingTurns, "linked_trace", env.TraceID)

	// A fresh tail tracker: its budget is derived from the (surviving) static
	// prefix markers, and it reseeds markers on the growing tail as the resumed
	// conversation continues.
	tail := newTailBreakpoints(params)

	// Cap the envelope's remaining budget at the live executor's configured
	// maxTurns. The turn budget is loop config, not part of the request digest,
	// so ValidateForResume cannot catch a mismatch: without the cap, an envelope
	// parked under a larger budget (an operator lowering maxTurns between park
	// and wake, or a tampered checkpoint) would resume with more turns than the
	// live configuration allows.
	response, err = e.runConversation(ctx, trace, params, tools, tail, env.Turn+1, min(env.RemainingTurns, e.maxTurns), nil)
	return response, err
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
	if len(params.Messages) == 0 {
		return errors.New("resume: provider state carries no messages")
	}
	last := &params.Messages[len(params.Messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		return fmt.Errorf("resume: last message role %q, want user (the slot the answer pairs into)", last.Role)
	}
	framed, err := checkpoint.FramedAnswers(pending, answers, 0)
	if err != nil {
		return err
	}
	for _, f := range framed {
		last.Content = append(last.Content, anthropic.ContentBlockParamUnion{
			OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: f.ID,
				Content: []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{Text: f.Text},
				}},
			},
		})
	}
	return nil
}

// resumeTracePrompt is the prompt string recorded on a resumed run's trace. The
// restored transcript is the real context; this string only labels the resume
// so the trace is self-describing.
func resumeTracePrompt(env checkpoint.Envelope) string {
	return fmt.Sprintf("resume suspended agent (key=%q, run=%q, reason=%q)", env.ReconcilerKey, env.RunID, env.Reason)
}
