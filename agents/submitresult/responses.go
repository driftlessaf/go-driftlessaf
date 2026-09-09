/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
)

// ResponsesMetadata describes the terminal function for the Responses protocol.
// Handler parses a provider-neutral call; the executor must validate an accepted
// outcome before committing it as the final result.
type ResponsesMetadata[Response any] struct {
	Definition responses.FunctionToolParam
	Handler    func(context.Context, toolcall.ToolCall, *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response]
}

// ResponsesTool constructs a native Responses submit function without a client
// or credentials. It shares payload parsing with the other protocol adapters.
func ResponsesTool[Response any](opts Options[Response]) (ResponsesMetadata[Response], error) {
	opts.setDefaults()
	s, err := opts.schemaForResponse()
	if err != nil {
		return ResponsesMetadata[Response]{}, fmt.Errorf("derive payload schema: %w", err)
	}
	s.Description = opts.PayloadDescription
	payload, err := schemaToMap(s)
	if err != nil {
		return ResponsesMetadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}
	return ResponsesMetadata[Response]{
		Definition: responses.FunctionToolParam{
			Name: opts.ToolName, Description: param.NewOpt(opts.Description),
			Parameters: openaiInputSchema(opts.PayloadFieldName, payload),
			// Existing schemas include optional properties. The executor enforces
			// schema and semantic validation before accepting a submission.
			Strict: param.NewOpt(false),
		},
		Handler: func(ctx context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
			return buildOutcome(ctx, opts, trace, call.ID, call.Name, call.Args)
		},
	}, nil
}
