/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"errors"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/chainguard-dev/clog"
)

// ClaudeTool constructs the Claude executor metadata for the submit_result tool.
func ClaudeTool[Response any](opts Options[Response]) (claudetool.SubmitMetadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return claudetool.SubmitMetadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return claudetool.SubmitMetadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{
				"input": toolUse.Input,
			}, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: errResp}
		}

		rawInputs := params.RawInputs()

		reasoning, errMap := claudetool.Param[string](params, "reasoning")
		if errMap != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, rawInputs, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: errMap}
		}

		payloadRaw, errMap := claudetool.Param[map[string]any](params, opts.PayloadFieldName)
		if errMap != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, rawInputs, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: errMap}
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		parsed, err := parsePayload[Response](payloadRaw)
		if err != nil {
			tc := trace.StartToolCall(toolUse.ID, toolUse.Name, rawInputs)
			tc.Complete(nil, err)
			return toolcall.SubmitOutcome[Response]{ToolResult: claudetool.Error("%v", err)}
		}

		return toolcall.SubmitOutcome[Response]{
			Accepted:   true,
			Response:   parsed,
			Reasoning:  reasoning,
			ToolResult: successResult(opts.SuccessMessage),
		}
	}

	return claudetool.SubmitMetadata[Response]{
		Definition: anthropic.ToolParam{
			Name:        opts.ToolName,
			Description: anthropic.String(opts.Description),
			InputSchema: claudeInputSchema(opts.PayloadFieldName, payloadSchema),
		},
		Handler: handler,
	}, nil
}

// claudeInputSchema builds the {reasoning, <payload>} input schema for the
// terminal submit_result tool.
func claudeInputSchema(payloadFieldName string, payloadSchema map[string]any) anthropic.ToolInputSchemaParam {
	return anthropic.ToolInputSchemaParam{
		Type: constant.Object("object"),
		Properties: map[string]any{
			"reasoning": map[string]any{
				"type":        "string",
				"description": "Explain why you are confident this result is complete and accurate.",
			},
			payloadFieldName: payloadSchema,
		},
		Required: []string{"reasoning", payloadFieldName},
	}
}

// ClaudeToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func ClaudeToolForResponse[Response any]() (claudetool.SubmitMetadata[Response], error) {
	return ClaudeTool(OptionsForResponse[Response]())
}
