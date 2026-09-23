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
		clog.InfoContext(ctx, "Submission rejected by result validators",
			"findings", len(findings), "rejected_by", rejectedBy(findings))
		rec.RecordToolCall(ctx, "submit_result_rejected")
		tc.CompleteRejected(fmt.Errorf("result rejected: validation raised %d finding(s)", len(findings)))
		return callbacks.RejectionResult(submitToolName, findings), false, nil
	}

	*resultPtr = outcome.Response
	// Marked terminal, not merely successful: this call commits the result,
	// so consumers counting successful work must not count it.
	tc.CompleteTerminal(outcome.ToolResult)
	return outcome.ToolResult, true, nil
}

// maxRejectedBy bounds the rendered list: a lint reviewer can raise hundreds of
// distinct path:line identifiers, and an unbounded line can exceed the log
// backend's per-entry limit.
const maxRejectedBy = 20

// maxIdentifierRunes bounds one rendered identifier; identifierTailRunes keeps
// its end, where a rule:path:line identifier carries the line number. Head-only
// truncation renders two findings in one long-path file identically.
const (
	maxIdentifierRunes  = 120
	identifierTailRunes = 40
)

// rejectedBy names the distinct checks that rejected a submission, each with the
// number of findings it raised, in the order the validators raised them.
//
// Identifiers only, never Finding.Details: a reviewer gate builds Details from
// analyzer output that quotes the source under review. Identifiers are
// validator-chosen, never model-supplied.
func rejectedBy(findings []callbacks.Finding) []string {
	counts := make(map[string]int, len(findings))
	order := make([]string, 0, len(findings))
	for _, f := range findings {
		if _, seen := counts[f.Identifier]; !seen {
			order = append(order, f.Identifier)
		}
		counts[f.Identifier]++
	}
	shown := min(len(order), maxRejectedBy)
	out := make([]string, 0, shown+1)
	hidden := 0
	for i, id := range order {
		if i >= shown {
			hidden += counts[id]
			continue
		}
		// %q keeps an empty or space-bearing identifier legible.
		out = append(out, fmt.Sprintf("%q x%d", truncated(id), counts[id]))
	}
	if len(order) > shown {
		// Both counts: the elided check often carries the most findings.
		out = append(out, fmt.Sprintf("and %d more check(s), %d finding(s)", len(order)-shown, hidden))
	}
	return out
}

// truncated caps one identifier, keeping both ends and cutting on rune
// boundaries so %q never renders a split character.
func truncated(id string) string {
	r := []rune(id)
	if len(r) <= maxIdentifierRunes {
		return id
	}
	return string(r[:maxIdentifierRunes-identifierTailRunes]) + "…" + string(r[len(r)-identifierTailRunes:])
}
