/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package toolcall

// SubmitOutcome is the result of a terminal submit tool handler processing a
// single tool call. Unlike regular tool handlers, a submit handler performs no
// side effects on the run: it parses the call into the outcome, and the
// executor decides whether the response commits (ending the run) — after the
// registered result validators accept it — or is rejected back to the model.
type SubmitOutcome[Response any] struct {
	// Accepted reports whether the call's payload parsed into a Response.
	// False means a parameter or parse failure; the handler has already
	// recorded the failed call on the trace, and ToolResult carries the error
	// to return to the model so it can correct the call and try again.
	Accepted bool

	// Response is the parsed result. Meaningful only when Accepted is true.
	Response Response

	// Reasoning is the model's justification for why it believes the result
	// is complete and accurate (the submit tool's universal reasoning
	// parameter). Meaningful only when Accepted is true.
	Reasoning string

	// ToolResult is the tool result payload to return to the model: the
	// parameter or parse error when Accepted is false, or the success payload
	// when Accepted is true. Executors return the success payload only after
	// the registered result validators accept the response; a rejected
	// submission returns the validators' findings instead.
	ToolResult map[string]any
}
