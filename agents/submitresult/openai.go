/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"context"
	"encoding/json"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/openai/openai-go/v3"
	oaiparam "github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAITool constructs the OpenAI executor metadata for the submit_result tool.
func OpenAITool[Response any](opts Options[Response]) (openaistool.SubmitMetadata[Response], error) {
	opts.setDefaults()

	props, required, flat, err := toolParameters(opts, schemaToAny)
	if err != nil {
		return openaistool.SubmitMetadata[Response]{}, err
	}

	handler := func(ctx context.Context, tc openai.ChatCompletionMessageToolCallUnion, trace *agenttrace.Trace[Response]) toolcall.SubmitOutcome[Response] {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			// Recoverable, like every other submit rejection: the parse error
			// goes back to the model as a corrective hint, and a run that
			// never lands a parseable submit cannot commit a result at all.
			trace.RejectedToolCall(tc.ID, tc.Function.Name, map[string]any{"arguments": tc.Function.Arguments}, fmt.Errorf("%w: failed to parse tool arguments: %w", ErrParameter, err))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("Failed to parse tool arguments: %v", err)}
		}
		return buildOutcome(ctx, opts, flat, trace, tc.ID, tc.Function.Name, args)
	}

	return openaistool.SubmitMetadata[Response]{
		Definition: openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        opts.ToolName,
				Description: oaiparam.NewOpt(opts.Description),
				Parameters:  openaiInputSchema(props, required, opts.OmitReasoning),
			},
		},
		Handler: handler,
	}, nil
}

// openaiInputSchema builds the {reasoning, <payload>} parameters for the
// terminal submit_result tool from the parameters toolParameters renders.
func openaiInputSchema(properties map[string]any, required []string, omitReasoning bool) shared.FunctionParameters {
	input := shared.FunctionParameters{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
	if !omitReasoning {
		properties["reasoning"] = map[string]any{
			"type":        "string",
			"description": reasoningDescription,
		}
		input["required"] = append([]string{"reasoning"}, required...)
	}
	return input
}

// OpenAIToolForResponse constructs the submit_result tool using metadata inferred from
// the response type annotations.
func OpenAIToolForResponse[Response any]() (openaistool.SubmitMetadata[Response], error) {
	return OpenAITool(OptionsForResponse[Response]())
}
