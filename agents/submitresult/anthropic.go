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
	"reflect"

	"chainguard.dev/driftlessaf/agents/evals"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/chainguard-dev/clog"
)

// ClaudeTool constructs the Claude executor metadata for the submit_result tool.
func ClaudeTool[Response any](opts Options[Response]) (claudeexecutor.ToolMetadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return claudeexecutor.ToolMetadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	payloadSchema, err := schemaToMap(responseSchema)
	if err != nil {
		return claudeexecutor.ToolMetadata[Response]{}, fmt.Errorf("convert payload schema: %w", err)
	}

	handler := func(ctx context.Context, toolUse anthropic.ToolUseBlock, trace *evals.Trace[Response], result *Response) map[string]any {
		log := clog.FromContext(ctx)

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
			return errMap
		}

		log.With("reasoning", reasoning).Info("Submitting result")

		tc := trace.StartToolCall(toolUse.ID, toolUse.Name, rawInputs)

		payloadJSON, err := json.Marshal(payloadRaw)
		if err != nil {
			tc.Complete(nil, err)
			return claudetool.Error("failed to marshal payload: %v", err)
		}

		typ := reflect.TypeFor[Response]()
		var dest any
		if typ.Kind() == reflect.Pointer {
			dest = reflect.New(typ.Elem()).Interface()
		} else {
			dest = reflect.New(typ).Interface()
		}

		if err := json.Unmarshal(payloadJSON, dest); err != nil {
			tc.Complete(nil, err)
			return claudetool.Error("failed to unmarshal payload: %v", err)
		}

		var parsed Response
		if typ.Kind() == reflect.Pointer {
			parsed = dest.(Response)
		} else {
			parsed = reflect.ValueOf(dest).Elem().Interface().(Response)
		}

		*result = parsed

		success := map[string]any{
			"success": true,
			"message": opts.SuccessMessage,
		}

		tc.Complete(success, nil)
		return success
	}

	return claudeexecutor.ToolMetadata[Response]{
		Definition: anthropic.ToolParam{
			Name:        opts.ToolName,
			Description: anthropic.String(opts.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type: constant.Object("object"),
				Properties: map[string]any{
					"reasoning": map[string]any{
						"type":        "string",
						"description": "Explain why you are confident this result is complete and accurate.",
					},
					opts.PayloadFieldName: payloadSchema,
				},
				Required: []string{"reasoning", opts.PayloadFieldName},
			},
		},
		Handler: handler,
	}, nil
}

// ClaudeToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func ClaudeToolForResponse[Response any]() (claudeexecutor.ToolMetadata[Response], error) {
	return ClaudeTool(OptionsForResponse[Response]())
}
