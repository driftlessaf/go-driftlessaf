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
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
	"github.com/openai/openai-go"
	oaiparam "github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// OpenAITool constructs the OpenAI executor metadata for the submit_result tool.
func OpenAITool[Response any](opts Options[Response]) (openaistool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return openaistool.Metadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return openaistool.Metadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, tc openai.ChatCompletionMessageToolCall, trace *agenttrace.Trace[Response], result *Response) map[string]any {
		var inputMap map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &inputMap); err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, map[string]any{"arguments": tc.Function.Arguments}, errors.New("parameter error"))
			return params.Error("Failed to parse tool arguments: %v", err)
		}

		reasoning, err := params.Extract[string](inputMap, "reasoning")
		if err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, inputMap, errors.New("parameter error"))
			return params.Error("%s", err)
		}

		payloadRaw, err := params.Extract[map[string]any](inputMap, opts.PayloadFieldName)
		if err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, inputMap, errors.New("parameter error"))
			return withValidateHint(params.Error("%s", err), opts.ValidateToolName, opts.ToolName)
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		tc2 := trace.StartToolCall(tc.ID, tc.Function.Name, inputMap)

		parsed, err := parsePayload[Response](payloadRaw)
		if err != nil {
			tc2.Complete(nil, err)
			return withValidateHint(params.Error("%v", err), opts.ValidateToolName, opts.ToolName)
		}

		*result = parsed

		success := map[string]any{
			"success": true,
			"message": opts.SuccessMessage,
		}
		tc2.Complete(success, nil)
		return success
	}

	return openaistool.Metadata[Response]{
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

// openaiInputSchema builds the {reasoning, <payload>} parameters shared by the
// terminal submit_result tool and the non-terminal validate tool.
func openaiInputSchema(payloadFieldName string, payloadSchema map[string]any) shared.FunctionParameters {
	return shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"reasoning": map[string]any{
				"type":        "string",
				"description": "Explain why you are confident this result is complete and accurate.",
			},
			payloadFieldName: payloadSchema,
		},
		"required": []string{"reasoning", payloadFieldName},
	}
}

// OpenAIValidateTool constructs the non-terminal validate tool for the OpenAI
// executor. It takes submit_result's identical schema and reports whether a
// payload would be accepted, without setting the run's final result.
func OpenAIValidateTool[Response any](opts Options[Response]) (openaistool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return openaistool.Metadata[Response]{}, err
	}

	name := opts.ValidateToolName
	if name == "" {
		name = defaultValidateToolName
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription
	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return openaistool.Metadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, tc openai.ChatCompletionMessageToolCall, trace *agenttrace.Trace[Response], _ *Response) map[string]any {
		var inputMap map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &inputMap); err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, map[string]any{"arguments": tc.Function.Arguments}, errors.New("parameter error"))
			return params.Error("Failed to parse tool arguments: %v", err)
		}

		payloadRaw, err := params.Extract[map[string]any](inputMap, opts.PayloadFieldName)
		if err != nil {
			trace.BadToolCall(tc.ID, tc.Function.Name, inputMap, errors.New("parameter error"))
			return params.Error("%s", err)
		}

		tc2 := trace.StartToolCall(tc.ID, tc.Function.Name, inputMap)
		if _, err := parsePayload[Response](payloadRaw); err != nil {
			tc2.Complete(nil, err)
			return params.Error("%v", err)
		}

		success := validateSuccess(opts.ToolName)
		tc2.Complete(success, nil)
		return success
	}

	return openaistool.Metadata[Response]{
		Definition: openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        name,
				Description: oaiparam.NewOpt("Check whether a result payload would be accepted by " + opts.ToolName + ", without ending the run. Takes the identical schema as " + opts.ToolName + ". Use this to verify your payload's shape if you are unsure; it returns a validation error or confirms the payload is valid. It never returns a final answer — call " + opts.ToolName + " for that."),
				Parameters:  openaiInputSchema(opts.PayloadFieldName, payloadSchema),
			},
		},
		Handler: handler,
	}, nil
}

// OpenAIToolForResponse constructs the submit_result tool using metadata inferred from
// the response type annotations.
func OpenAIToolForResponse[Response any]() (openaistool.Metadata[Response], error) {
	return OpenAITool(OptionsForResponse[Response]())
}

// OpenAISubmitAndValidateForResponse constructs the terminal submit_result tool
// and its non-terminal validate companion from one Options, so they share an
// identical schema and submit_result's payload errors point at the validate tool.
func OpenAISubmitAndValidateForResponse[Response any]() (submit, validate openaistool.Metadata[Response], err error) {
	opts := OptionsForResponse[Response]()
	opts.setDefaults()
	opts.ValidateToolName = defaultValidateToolName

	submit, err = OpenAITool(opts)
	if err != nil {
		return openaistool.Metadata[Response]{}, openaistool.Metadata[Response]{}, err
	}
	validate, err = OpenAIValidateTool(opts)
	if err != nil {
		return openaistool.Metadata[Response]{}, openaistool.Metadata[Response]{}, err
	}
	return submit, validate, nil
}
