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
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"
)

// GoogleTool constructs the Google executor metadata for the submit_result tool.
func GoogleTool[Response any](opts Options[Response]) (googletool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return googletool.Metadata[Response]{}, err
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription

	genaiPayload := schemaToGenai(responseSchema)
	if genaiPayload == nil {
		return googletool.Metadata[Response]{}, fmt.Errorf("failed to derive payload schema")
	}

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *evals.Trace[Response], result *Response) *genai.FunctionResponse {
		log := clog.FromContext(ctx)

		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return errResp
		}

		payloadRaw, errResp := googletool.Param[map[string]any](call, opts.PayloadFieldName)
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return errResp
		}

		log.With("reasoning", reasoning).Info("Submitting result")

		tc := trace.StartToolCall(call.ID, call.Name, call.Args)

		payloadJSON, err := json.Marshal(payloadRaw)
		if err != nil {
			tc.Complete(nil, err)
			return googletool.Error(call, "failed to marshal payload: %v", err)
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
			return googletool.Error(call, "failed to unmarshal payload: %v", err)
		}

		var parsed Response
		if typ.Kind() == reflect.Pointer {
			parsed = dest.(Response)
		} else {
			parsed = reflect.ValueOf(dest).Elem().Interface().(Response)
		}

		*result = parsed

		response := &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: map[string]any{
				"success": true,
				"message": opts.SuccessMessage,
			},
		}

		tc.Complete(response.Response, nil)
		return response
	}

	return googletool.Metadata[Response]{
		Definition: &genai.FunctionDeclaration{
			Name:        opts.ToolName,
			Description: opts.Description,
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"reasoning": {
						Type:        genai.TypeString,
						Description: "Explain why you are confident this result is complete and accurate.",
					},
					opts.PayloadFieldName: genaiPayload,
				},
				Required: []string{"reasoning", opts.PayloadFieldName},
			},
		},
		Handler: handler,
	}, nil
}

// GoogleToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func GoogleToolForResponse[Response any]() (googletool.Metadata[Response], error) {
	return GoogleTool(OptionsForResponse[Response]())
}
