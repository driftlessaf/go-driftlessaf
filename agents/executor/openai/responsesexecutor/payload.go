/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package responsesexecutor

import (
	"encoding/json"
	"errors"
)

// safePayload preserves prompts, tool calls, text, and usage for opt-in trace
// payloads, but removes the opaque provider state that is only needed on wire.
func safePayload(value any) (any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("responses payload cannot be encoded")
	}
	if len(b) > maxPayloadBytes {
		return nil, errors.New("responses payload exceeds byte limit")
	}
	var payload any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, errors.New("responses payload cannot be decoded")
	}
	redactReasoning(payload)
	return payload, nil
}

func redactReasoning(value any) {
	switch value := value.(type) {
	case map[string]any:
		delete(value, "encrypted_content")
		for _, v := range value {
			redactReasoning(v)
		}
	case []any:
		for _, v := range value {
			redactReasoning(v)
		}
	}
}
