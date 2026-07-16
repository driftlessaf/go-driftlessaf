/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/openai/openai-go"
	oaiparam "github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// OpenAITool constructs the OpenAI executor metadata for the submit_result tool.
func OpenAITool[Response any](opts Options[Response]) (openaistool.SubmitMetadata[Response], error) {
	opts.setDefaults()

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return openaistool.SubmitMetadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, tc openai.ChatCompletionMessageToolCall, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, map[string]any{"arguments": tc.Function.Arguments}, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("Failed to parse tool arguments: %v", err)}
		}
		return buildOutcome(ctx, opts, trace, tc.ID, tc.Function.Name, args)
	}

	return openaistool.SubmitMetadata[Response]{
		Definition: openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        opts.ToolName,
				Description: oaiparam.NewOpt(opts.Description),
				Parameters:  openaiInputSchema(opts.PayloadFieldName, payloadSchema),
			},
		},
		Handler: handler,
	}, nil
}

// openaiInputSchema builds the {reasoning, <payload>} parameters for the
// terminal submit_result tool.
func openaiInputSchema(payloadFieldName string, payloadSchema map[string]any) shared.FunctionParameters {
	return shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"reasoning": map[string]any{
				"type":        "string",
				"description": reasoningDescription,
			},
			payloadFieldName: payloadSchema,
		},
		"required": []string{"reasoning", payloadFieldName},
	}
}

// OpenAIToolForResponse constructs the submit_result tool using metadata inferred from
// the response type annotations.
func OpenAIToolForResponse[Response any]() (openaistool.SubmitMetadata[Response], error) {
	return OpenAITool(OptionsForResponse[Response]())
}
