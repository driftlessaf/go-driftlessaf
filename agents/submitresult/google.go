/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"errors"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
)

// GoogleTool constructs the Google executor metadata for the submit_result tool.
func GoogleTool[Response any](opts Options[Response]) (googletool.SubmitMetadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return googletool.SubmitMetadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	genaiPayload := schemaToGenai(responseSchema)
	if genaiPayload == nil {
		return googletool.SubmitMetadata[Response]{}, errors.New("failed to derive payload schema")
	}

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		reasoning, err := params.Extract[string](call.Args, "reasoning")
		if err != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
		}

		payloadRaw, err := params.Extract[map[string]any](call.Args, opts.PayloadFieldName)
		if err != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		parsed, err := parsePayload[Response](payloadRaw)
		if err != nil {
			tc := trace.StartToolCall(call.ID, call.Name, call.Args)
			tc.Complete(nil, err)
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%v", err)}
		}

		return toolcall.SubmitOutcome[Response]{
			Accepted:   true,
			Response:   parsed,
			Reasoning:  reasoning,
			ToolResult: successResult(opts.SuccessMessage),
		}
	}

	return googletool.SubmitMetadata[Response]{
		Definition: &genai.FunctionDeclaration{
			Name:        opts.ToolName,
			Description: opts.Description,
			Parameters:  googleInputSchema(opts.PayloadFieldName, genaiPayload),
		},
		Handler: handler,
	}, nil
}

// googleInputSchema builds the {reasoning, <payload>} schema for the terminal
// submit_result tool.
func googleInputSchema(payloadFieldName string, payloadSchema *genai.Schema) *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"reasoning": {
				Type:        genai.TypeString,
				Description: "Explain why you are confident this result is complete and accurate.",
			},
			payloadFieldName: payloadSchema,
		},
		Required: []string{"reasoning", payloadFieldName},
	}
}

// GoogleToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func GoogleToolForResponse[Response any]() (googletool.SubmitMetadata[Response], error) {
	return GoogleTool(OptionsForResponse[Response]())
}
