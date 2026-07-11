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

// parsePayload converts a raw payload object (as received from the model) into
// the strongly-typed Response. It is shared by the per-provider submit tool
// handlers so all apply exactly the same parsing rules.
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

// successResult is the tool result an accepted submission carries back toward
// the model. The executor returns it only after the registered result
// validators accept the response.
func successResult(successMessage string) map[string]any {
	return map[string]any{
		"success": true,
		"message": successMessage,
	}
}
