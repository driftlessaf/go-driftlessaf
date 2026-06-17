/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package submitresult

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// defaultValidateToolName is the tool name used for the non-terminal companion
// to submit_result. The validate tool takes the identical schema and reports
// whether a payload would be accepted, without ending the agent loop.
const defaultValidateToolName = "validate_result"

// parsePayload converts a raw payload object (as received from the model) into
// the strongly-typed Response. It is shared by the terminal submit tool and the
// non-terminal validate tool so both apply exactly the same parsing rules.
func parsePayload[Response any](payloadRaw map[string]any) (Response, error) {
	var zero Response

	payloadJSON, err := json.Marshal(payloadRaw)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal payload: %w", err)
	}

	typ := reflect.TypeFor[Response]()
	var dest any
	if typ.Kind() == reflect.Pointer {
		dest = reflect.New(typ.Elem()).Interface()
	} else {
		dest = reflect.New(typ).Interface()
	}

	if err := json.Unmarshal(payloadJSON, dest); err != nil {
		return zero, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	if typ.Kind() == reflect.Pointer {
		return dest.(Response), nil
	}
	return reflect.ValueOf(dest).Elem().Interface().(Response), nil
}

// withValidateHint appends guidance to a submit tool's error response pointing
// the model at the non-terminal validate tool, so it can check a payload
// without risking termination on a bad submit. It is a no-op when no validate
// tool is registered (validateToolName == "").
func withValidateHint(errResp map[string]any, validateToolName, submitToolName string) map[string]any {
	if validateToolName == "" || errResp == nil {
		return errResp
	}
	if msg, ok := errResp["error"].(string); ok {
		errResp["error"] = msg + fmt.Sprintf(" You may call %s with the same arguments to check your payload first; it validates without ending the run. %s is terminal, so only call it once you have the correct shape.", validateToolName, submitToolName)
	}
	return errResp
}

// validateSuccess is the response a validate tool returns for an acceptable
// payload. It deliberately does not set the run's final result.
func validateSuccess(submitToolName string) map[string]any {
	return map[string]any{
		"valid":   true,
		"message": fmt.Sprintf("Payload is valid. Call %s with these same arguments to finish.", submitToolName),
	}
}
