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

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response], result *Response) *genai.FunctionResponse {
		reasoning, errResp := googletool.Param[string](call, "reasoning")
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return errResp
		}

		payloadRaw, errResp := googletool.Param[map[string]any](call, opts.PayloadFieldName)
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return googleWithValidateHint(errResp, opts.ValidateToolName, opts.ToolName)
		}

		clog.InfoContext(ctx, "Submitting result",
			"reasoning", reasoning,
		)

		tc := trace.StartToolCall(call.ID, call.Name, call.Args)

		parsed, err := parsePayload[Response](payloadRaw)
		if err != nil {
			tc.Complete(nil, err)
			return googleWithValidateHint(googletool.Error(call, "%v", err), opts.ValidateToolName, opts.ToolName)
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
			Parameters:  googleInputSchema(opts.PayloadFieldName, genaiPayload),
		},
		Handler: handler,
	}, nil
}

// googleInputSchema builds the {reasoning, <payload>} schema shared by the
// terminal submit_result tool and the non-terminal validate tool.
func googleInputSchema(payloadFieldName string, payloadSchema *genai.Schema) *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"reasoning": {
				Type:        genai.TypeString,
				Description: "Explain why you are confident this result is complete and accurate.",
			},
			payloadFieldName: payloadSchema,
		},
		Required: []string{"reasoning", payloadFieldName},
	}
}

// googleWithValidateHint appends the validate-tool hint to a submit tool's error
// FunctionResponse, mirroring withValidateHint for the generic error maps.
func googleWithValidateHint(resp *genai.FunctionResponse, validateToolName, submitToolName string) *genai.FunctionResponse {
	if validateToolName == "" || resp == nil {
		return resp
	}
	resp.Response = withValidateHint(resp.Response, validateToolName, submitToolName)
	return resp
}

// GoogleValidateTool constructs the non-terminal validate tool for the Google
// executor. It takes submit_result's identical schema and reports whether a
// payload would be accepted, without setting the run's final result.
func GoogleValidateTool[Response any](opts Options[Response]) (googletool.Metadata[Response], error) {
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return googletool.Metadata[Response]{}, err
	}

	name := opts.ValidateToolName
	if name == "" {
		name = defaultValidateToolName
	}

	responseSchema := opts.schemaForResponse()
	responseSchema.Description = opts.PayloadDescription
	genaiPayload := schemaToGenai(responseSchema)
	if genaiPayload == nil {
		return googletool.Metadata[Response]{}, fmt.Errorf("failed to derive payload schema")
	}

	handler := func(ctx context.Context, call *genai.FunctionCall, trace *agenttrace.Trace[Response], _ *Response) *genai.FunctionResponse {
		payloadRaw, errResp := googletool.Param[map[string]any](call, opts.PayloadFieldName)
		if errResp != nil {
			trace.BadToolCall(call.ID, call.Name, call.Args, errors.New("parameter error"))
			return errResp
		}

		tc := trace.StartToolCall(call.ID, call.Name, call.Args)
		if _, err := parsePayload[Response](payloadRaw); err != nil {
			tc.Complete(nil, err)
			return googletool.Error(call, "%v", err)
		}

		success := validateSuccess(opts.ToolName)
		tc.Complete(success, nil)
		return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: success}
	}

	return googletool.Metadata[Response]{
		Definition: &genai.FunctionDeclaration{
			Name:        name,
			Description: "Check whether a result payload would be accepted by " + opts.ToolName + ", without ending the run. Takes the identical schema as " + opts.ToolName + ". Use this to verify your payload's shape if you are unsure; it returns a validation error or confirms the payload is valid. It never returns a final answer — call " + opts.ToolName + " for that.",
			Parameters:  googleInputSchema(opts.PayloadFieldName, genaiPayload),
		},
		Handler: handler,
	}, nil
}

// GoogleToolForResponse constructs the submit_result tool using metadata inferred from the
// response type annotations.
func GoogleToolForResponse[Response any]() (googletool.Metadata[Response], error) {
	return GoogleTool(OptionsForResponse[Response]())
}

// GoogleSubmitAndValidateForResponse constructs the terminal submit_result tool
// and its non-terminal validate companion from one Options, so they share an
// identical schema and submit_result's payload errors point at the validate tool.
func GoogleSubmitAndValidateForResponse[Response any]() (submit, validate googletool.Metadata[Response], err error) {
	opts := OptionsForResponse[Response]()
	opts.setDefaults()
	opts.ValidateToolName = defaultValidateToolName

	submit, err = GoogleTool(opts)
	if err != nil {
		return googletool.Metadata[Response]{}, googletool.Metadata[Response]{}, err
	}
	validate, err = GoogleValidateTool(opts)
	if err != nil {
		return googletool.Metadata[Response]{}, googletool.Metadata[Response]{}, err
	}
	return submit, validate, nil
}
