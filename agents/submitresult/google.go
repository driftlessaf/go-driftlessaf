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
	"google.golang.org/genai"
)

// GoogleTool constructs the Google executor metadata for the submit_result tool.
func GoogleTool[Response any](opts Options[Response]) (googletool.SubmitMetadata[Response], error) {
	opts.setDefaults()

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	genaiPayload := schemaToGenai(responseSchema)
	if genaiPayload == nil {
		return googletool.SubmitMetadata[Response]{}, errors.New("failed to derive payload schema")
	}

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		return buildOutcome(ctx, opts, trace, call.ID, call.Name, call.Args)
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
				Description: reasoningDescription,
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
