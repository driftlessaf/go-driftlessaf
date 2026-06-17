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
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/chainguard-dev/clog"
)

// ClaudeTool constructs the Claude executor metadata for the submit_result tool.
func ClaudeTool[Response any](opts Options[Response]) (claudetool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return claudetool.Metadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return claudetool.Metadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Response], result *Response) map[string]any {
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{
				"input": toolUse.Input,
			}, errors.New("parameter error"))
			return errResp
		}

		rawInputs := params.RawInputs()

		reasoning, errMap := claudetool.Param[string](params, "reasoning")
		if errMap != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, rawInputs, errors.New("parameter error"))
			return errMap
		}

		payloadRaw, errMap := claudetool.Param[map[string]any](params, opts.PayloadFieldName)
		if errMap != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, rawInputs, errors.New("parameter error"))
			return withValidateHint(errMap, opts.ValidateToolName, opts.ToolName)
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, rawInputs)

		parsed, err := parsePayload[Response](payloadRaw)
		if err != nil {
			tc.Complete(nil, err)
			return withValidateHint(claudetool.Error("%v", err), opts.ValidateToolName, opts.ToolName)
		}

		*result = parsed

		success := map[string]any{
			"success": true,
			"message": opts.SuccessMessage,
		}

		tc.Complete(success, nil)
		return success
	}

	return claudetool.Metadata[Response]{
		Definition: anthropic.ToolParam{
			Name:        opts.ToolName,
			Description: anthropic.String(opts.Description),
			InputSchema: claudeInputSchema(opts.PayloadFieldName, payloadSchema),
		},
		Handler: handler,
	}, nil
}

// claudeInputSchema builds the {reasoning, <payload>} input schema shared by the
// terminal submit_result tool and the non-terminal validate tool, so both
// advertise an identical shape.
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

// ClaudeValidateTool constructs the Claude executor metadata for the
// non-terminal validate tool. It takes the identical schema as submit_result
// and reports whether the payload would be accepted, without setting the run's
// final result — giving the model a safe way to check its payload's shape
// before the terminal submit.
func ClaudeValidateTool[Response any](opts Options[Response]) (claudetool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return claudetool.Metadata[Response]{}, err
	}

	name := opts.ValidateToolName
	if name == "" {
		name = defaultValidateToolName
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription
	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return claudetool.Metadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *agenttrace.Trace[Response], _ *Response) map[string]any {
		params, errResp := claudetool.NewParams(toolUse)
		if errResp != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, map[string]any{"input": toolUse.Input}, errors.New("parameter error"))
			return errResp
		}
		rawInputs := params.RawInputs()

		payloadRaw, errMap := claudetool.Param[map[string]any](params, opts.PayloadFieldName)
		if errMap != nil {
			trace.BadToolCall(toolUse.ID, toolUse.Name, rawInputs, errors.New("parameter error"))
			return errMap
		}

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, rawInputs)
		if _, err := parsePayload[Response](payloadRaw); err != nil {
			tc.Complete(nil, err)
			return claudetool.Error("%v", err)
		}

		success := validateSuccess(opts.ToolName)
		tc.Complete(success, nil)
		return success
	}

	return claudetool.Metadata[Response]{
		Definition: anthropic.ToolParam{
			Name:        name,
			Description: anthropic.String("Check whether a result payload would be accepted by " + opts.ToolName + ", without ending the run. Takes the identical schema as " + opts.ToolName + ". Use this to verify your payload's shape if you are unsure; it returns a validation error or confirms the payload is valid. It never returns a final answer — call " + opts.ToolName + " for that."),
			InputSchema: claudeInputSchema(opts.PayloadFieldName, payloadSchema),
		},
		Handler: handler,
	}, nil
}

// ClaudeToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func ClaudeToolForResponse[Response any]() (claudetool.Metadata[Response], error) {
	return ClaudeTool(OptionsForResponse[Response]())
}

// ClaudeSubmitAndValidateForResponse constructs the terminal submit_result tool
// and its non-terminal validate companion from one Options, so they share an
// identical schema and submit_result's payload errors point at the validate
// tool. Both are inferred from the response type annotations.
func ClaudeSubmitAndValidateForResponse[Response any]() (submit, validate claudetool.Metadata[Response], err error) {
	opts := OptionsForResponse[Response]()
	opts.setDefaults()
	opts.ValidateToolName = defaultValidateToolName

	submit, err = ClaudeTool(opts)
	if err != nil {
		return claudetool.Metadata[Response]{}, claudetool.Metadata[Response]{}, err
	}
	validate, err = ClaudeValidateTool(opts)
	if err != nil {
		return claudetool.Metadata[Response]{}, claudetool.Metadata[Response]{}, err
	}
	return submit, validate, nil
}
