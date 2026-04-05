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
	"reflect"

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
			return params.Error("%s", err)
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		tc2 := trace.StartToolCall(tc.ID, tc.Function.Name, inputMap)

		payloadJSON, err := json.Marshal(payloadRaw)
		if err != nil {
			tc2.Complete(nil, err)
			return params.Error("failed to marshal payload: %v", err)
		}

		typ := reflect.TypeFor[Response]()
		var dest any
		if typ.Kind() == reflect.Pointer {
			dest = reflect.New(typ.Elem()).Interface()
		} else {
			dest = reflect.New(typ).Interface()
		}

		if err := json.Unmarshal(payloadJSON, dest); err != nil {
			tc2.Complete(nil, err)
			return params.Error("failed to unmarshal payload: %v", err)
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
		tc2.Complete(success, nil)
		return success
	}

	return openaistool.Metadata[Response]{
		Definition: openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        opts.ToolName,
				Description: oaiparam.NewOpt(opts.Description),
				Parameters: shared.FunctionParameters{
					"type": "object",
					"properties": map[string]any{
						"reasoning": map[string]any{
							"type":        "string",
							"description": "Explain why you are confident this result is complete and accurate.",
						},
						opts.PayloadFieldName: payloadSchema,
					},
					"required": []string{"reasoning", opts.PayloadFieldName},
				},
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
