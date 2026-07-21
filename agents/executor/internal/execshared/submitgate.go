/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package execshared

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/internal/telemetry"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/chainguard-dev/clog"
)

// GateSubmission gates a terminal submit tool call on the registered result
// validators — the provider-neutral tail of every executor's
// evaluateSubmission, applied after the backend's submit handler has parsed
// the call into outcome. callID, toolName, and args identify the call on the
// trace; submitToolName names the tool in the rejection payload. It returns
// the tool result to send back to the model and whether the response
// committed as the run's final result (written through resultPtr). A rejected
// submission returns the validators' findings so the model can address them
// and submit again; a validator error aborts the run.
func GateSubmission[Response any](
	ctx context.Context,
	outcome toolcall.SubmitOutcome[Response],
	trace *agenttrace.Trace[Response],
	callID, toolName string,
	args map[string]any,
	validators []callbacks.ResultValidator[Response],
	rec *telemetry.Recorder,
	submitToolName string,
	resultPtr *Response,
) (map[string]any, bool, error) {
	if !outcome.Accepted {
		// The handler recorded the failed call on the trace; its ToolResult
		// carries the parameter/parse error back to the model.
		return outcome.ToolResult, false, nil
	}

	// The handler leaves accepted calls unrecorded so this trace call's
	// completion reflects the validation verdict.
	tc := trace.StartToolCall(callID, toolName, args)

	findings, err := callbacks.ValidateResult(ctx, validators, outcome.Response, outcome.Reasoning)
	if err != nil {
		err = fmt.Errorf("result validation: %w", err)
		tc.Complete(nil, err)
		return nil, false, err
	}
	if len(findings) > 0 {
		clog.InfoContext(ctx, "Submission rejected by result validators", "findings", len(findings))
		rec.RecordToolCall(ctx, "submit_result_rejected")
		tc.CompleteRejected(fmt.Errorf("result rejected: validation raised %d finding(s)", len(findings)))
		return callbacks.RejectionResult(submitToolName, findings), false, nil
	}

	*resultPtr = outcome.Response
	tc.Complete(outcome.ToolResult, nil)
	return outcome.ToolResult, true, nil
}
