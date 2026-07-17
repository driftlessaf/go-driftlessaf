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
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/params"
	"github.com/chainguard-dev/clog"
)

// reasoningDescription documents the reasoning parameter in every provider's
// submit tool schema.
const reasoningDescription = "Explain why you are confident this result is complete and accurate."

// buildOutcome turns decoded submit tool-call arguments into a SubmitOutcome.
// It is shared by the per-provider submit tool handlers, which differ only in
// how they acquire the argument map.
func buildOutcome[Response any](ctx context.Context, opts Options[Response], trace *agenttrace.Trace[Response], id, name string, args map[string]any) toolcall.SubmitOutcome[Response] {
	reasoning, err := params.Extract[string](args, "reasoning")
	if err != nil {
		trace.BadToolCall(id, name, args, errors.New("parameter error"))
		return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
	}

	payloadRaw, err := params.Extract[map[string]any](args, opts.PayloadFieldName)
	if err != nil {
		coerced, ok := coerceStringPayload(args, opts.PayloadFieldName)
		if !ok {
			trace.BadToolCall(id, name, args, errors.New("parameter error"))
			return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%s", err)}
		}
		clog.WarnContext(ctx, "Coerced stringified submit payload into an object",
			"tool", name,
			"field", opts.PayloadFieldName,
		)
		payloadRaw = coerced
	}

	clog.InfoContext(ctx, "Submitting result",
		"reasoning", reasoning,
	)

	parsed, err := parsePayload[Response](payloadRaw)
	if err != nil {
		tc := trace.StartToolCall(id, name, args)
		tc.Complete(nil, err)
		return toolcall.SubmitOutcome[Response]{ToolResult: params.Error("%v", err)}
	}

	return toolcall.SubmitOutcome[Response]{
		Accepted:   true,
		Response:   parsed,
		Reasoning:  reasoning,
		ToolResult: successResult(opts.SuccessMessage),
	}
}

// coerceStringPayload recovers the common model mistake of JSON-encoding the
// payload object into a string instead of passing it as a nested object. It
// reports ok when the field is a string containing a JSON object, returning
// the decoded object; callers fall back to the original extraction error
// otherwise, so the model still sees the type-mismatch hint.
func coerceStringPayload(args map[string]any, field string) (map[string]any, bool) {
	s, err := params.Extract[string](args, field)
	if err != nil {
		return nil, false
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// parsePayload converts a raw payload object (as received from the model) into
// the strongly-typed Response. It is shared by the per-provider submit tool
// handlers so all apply exactly the same parsing rules.
func parsePayload[Response any](payloadRaw map[string]any) (Response, error) {
	var zero Response

	payloadJSON, err := json.Marshal(payloadRaw)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal payload: %w", err)
	}

	dest := newResponseValue[Response]()
	if err := json.Unmarshal(payloadJSON, dest); err != nil {
		return zero, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	if reflect.TypeFor[Response]().Kind() == reflect.Pointer {
		return dest.(Response), nil
	}
	return reflect.ValueOf(dest).Elem().Interface().(Response), nil
}

// newResponseValue allocates a fresh Response for reflection-driven JSON
// work, dereferencing pointer types so the result addresses the object the
// model submits.
func newResponseValue[Response any]() any {
	typ := reflect.TypeFor[Response]()
	if typ.Kind() == reflect.Pointer {
		return reflect.New(typ.Elem()).Interface()
	}
	return reflect.New(typ).Interface()
}

// successResult is the tool result an accepted submission carries back toward
// the model. The executor returns it only after the registered result
// validators accept the response.
func successResult(successMessage string) map[string]any {
	return map[string]any{
		"success": true,
		"message": successMessage,
	}
}
