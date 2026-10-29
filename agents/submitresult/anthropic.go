/*
Copyright 2025 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"encoding/json"
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

	props, required, flat, err := toolParameters(opts, schemaToAny)
	if err != nil {
		return claudetool.SubmitMetadata[Response]{}, err
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		var args map[string]any
		if err := json.Unmarshal(toolUse.Input, &args); err != nil {
			// Recoverable, like every other submit rejection: the parse error
			// goes back to the model as a corrective hint, and a run that
			// never lands a parseable submit cannot commit a result at all.
			trace.RejectedToolCall(toolUse.ID, toolUse.Name, map[string]any{
				"input": toolUse.Input,
			}, fmt.Errorf("%w: failed to parse tool input: %w", ErrParameter, err))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("Failed to parse tool input: %v", err)}
		}
		return buildOutcome(ctx, opts, flat, trace, toolUse.ID, toolUse.Name, args)
	}

	return claudetool.SubmitMetadata[Response]{
		Definition: anthropic.ToolParam{
			Name:        opts.ToolName,
			Description: anthropic.String(opts.Description),
			InputSchema: claudeInputSchema(props, required, opts.OmitReasoning),
		},
		Handler: handler,
	}, nil
}

// claudeInputSchema builds the {reasoning, <payload>} input schema for the
// terminal submit_result tool from the parameters toolParameters renders.
func claudeInputSchema(properties map[string]any, required []string, omitReasoning bool) anthropic.ToolInputSchemaParam {
	input := anthropic.ToolInputSchemaParam{
		Type:       constant.Object("object"),
		Properties: properties,
		Required:   required,
	}
	if !omitReasoning {
		properties["reasoning"] = map[string]any{
			"type":        "string",
			"description": reasoningDescription,
		}
		input.Required = append([]string{"reasoning"}, required...)
	}
	return input
}

// ClaudeToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func ClaudeToolForResponse[Response any]() (claudetool.SubmitMetadata[Response], error) {
	return ClaudeTool(OptionsForResponse[Response]())
}
