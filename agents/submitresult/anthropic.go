/*
Copyright 2025 Chainguard, Inc.
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
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// ClaudeTool constructs the Claude executor metadata for the submit_result tool.
func ClaudeTool[Response any](opts Options[Response]) (claudetool.SubmitMetadata[Response], error) {
	opts.setDefaults()

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return claudetool.SubmitMetadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		var args map[string]any
		if err := json.Unmarshal(toolUse.Input, &args); err != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{
				"input": toolUse.Input,
			}, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("Failed to parse tool input: %v", err)}
		}
		return buildOutcome(ctx, opts, trace, toolUse.ID, toolUse.Name, args)
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
				"description": reasoningDescription,
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
