/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"encoding/json"
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
)

// sealSensitiveTraceFields replaces the free-text payload fields of a marshalled
// trace-event JSON (input_prompt, result, tool_calls[].params, tool_calls[].result,
// reasoning[].thinking) with sealed envelopes, leaving structural fields (ids,
// model, agent_name, source, tokens, timings, exec_context, errors) in plaintext
// so cost views, the dashboard, and the MCP keep working without decrypt.
//
// The trace-level `metadata` (map[string]any) is by contract structural-only —
// annotations set by the framework/executor, not free-form submission content —
// so it is intentionally NOT sealed. Do not place submission-derived free text in
// metadata while payload encryption is relied upon; put it in a sealed field.
//
// turns[].system is likewise left plaintext: despite the name it is the OTel
// GenAI provider identifier ("anthropic", "google.vertex", "openai"; see
// Trace.BeginTurn), a low-cardinality structural label that powers provider
// filtering — not the LLM system prompt. It carries no submission-derived text.
//
// One sealing session is used for the whole event so every field shares a single
// KMS-wrapped DEK (one KMS call per event). An error is returned rather than
// falling back to plaintext — callers must fail closed and drop the event.
func sealSensitiveTraceFields(ctx context.Context, enc *payloadcrypt.Encryptor, raw []byte) ([]byte, error) {
	sess, err := enc.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("new seal session: %w", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal trace event: %w", err)
	}

	// input_prompt is a STRING column: keep the sealed value a JSON string.
	if err := sealStringField(sess, obj, "input_prompt"); err != nil {
		return nil, err
	}
	// result is a JSON column: the sealed envelope object is itself valid JSON.
	if err := sealJSONField(sess, obj, "result"); err != nil {
		return nil, err
	}
	// tool_calls[].params / .result are JSON columns.
	if err := sealObjectArrayFields(sess, obj, "tool_calls", []string{"params", "result"}, nil); err != nil {
		return nil, err
	}
	// reasoning[].thinking is a STRING column.
	if err := sealObjectArrayFields(sess, obj, "reasoning", nil, []string{"thinking"}); err != nil {
		return nil, err
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal sealed trace event: %w", err)
	}
	return out, nil
}

// sealSensitiveSpanFields seals the per-turn payload fields of a marshalled
// span-event JSON (prompt_messages, completion), both JSON columns. The span's
// `metadata` is structural-only by the same contract as the trace-level metadata
// (see sealSensitiveTraceFields) and is intentionally left plaintext — its
// "system" key is the OTel provider identifier (see Trace.BeginTurn), not the
// LLM system prompt, so it carries no submission-derived free text.
func sealSensitiveSpanFields(ctx context.Context, enc *payloadcrypt.Encryptor, raw []byte) ([]byte, error) {
	sess, err := enc.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("new seal session: %w", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal span event: %w", err)
	}

	if err := sealJSONField(sess, obj, "prompt_messages"); err != nil {
		return nil, err
	}
	if err := sealJSONField(sess, obj, "completion"); err != nil {
		return nil, err
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal sealed span event: %w", err)
	}
	return out, nil
}

// isNullOrAbsent reports whether the field is missing or JSON null — nothing to
// seal in either case.
func isNullOrAbsent(v json.RawMessage, ok bool) bool {
	return !ok || len(v) == 0 || string(v) == "null"
}

// sealJSONField replaces a JSON-typed field's value with the sealed envelope
// object (still valid JSON). The sealed plaintext is the field's raw JSON bytes,
// so Open recovers the exact original value.
func sealJSONField(sess *payloadcrypt.Session, obj map[string]json.RawMessage, key string) error {
	v, ok := obj[key]
	if isNullOrAbsent(v, ok) {
		return nil
	}
	env, err := sess.Seal(v)
	if err != nil {
		return fmt.Errorf("seal %q: %w", key, err)
	}
	obj[key] = env
	return nil
}

// sealStringField replaces a STRING-typed field's value with the sealed envelope
// encoded AS a JSON string, so the recorder can still load it into a STRING
// column. The sealed plaintext is the field's raw JSON bytes (the quoted string).
func sealStringField(sess *payloadcrypt.Session, obj map[string]json.RawMessage, key string) error {
	v, ok := obj[key]
	if isNullOrAbsent(v, ok) {
		return nil
	}
	env, err := sess.Seal(v)
	if err != nil {
		return fmt.Errorf("seal %q: %w", key, err)
	}
	// Encode the envelope JSON as a JSON string value.
	quoted, err := json.Marshal(string(env))
	if err != nil {
		return fmt.Errorf("encode sealed %q as string: %w", key, err)
	}
	obj[key] = quoted
	return nil
}

// sealObjectArrayFields seals named fields inside each element of a JSON array of
// objects (e.g. tool_calls, reasoning). jsonKeys are sealed as JSON values,
// stringKeys as JSON strings.
func sealObjectArrayFields(sess *payloadcrypt.Session, obj map[string]json.RawMessage, arrayKey string, jsonKeys, stringKeys []string) error {
	v, ok := obj[arrayKey]
	if isNullOrAbsent(v, ok) {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(v, &elems); err != nil {
		return fmt.Errorf("unmarshal %q array: %w", arrayKey, err)
	}
	for i, elem := range elems {
		var em map[string]json.RawMessage
		if err := json.Unmarshal(elem, &em); err != nil {
			return fmt.Errorf("unmarshal %q[%d]: %w", arrayKey, i, err)
		}
		for _, k := range jsonKeys {
			if err := sealJSONField(sess, em, k); err != nil {
				return fmt.Errorf("%q[%d]: %w", arrayKey, i, err)
			}
		}
		for _, k := range stringKeys {
			if err := sealStringField(sess, em, k); err != nil {
				return fmt.Errorf("%q[%d]: %w", arrayKey, i, err)
			}
		}
		reencoded, err := json.Marshal(em)
		if err != nil {
			return fmt.Errorf("marshal sealed %q[%d]: %w", arrayKey, i, err)
		}
		elems[i] = reencoded
	}
	reencoded, err := json.Marshal(elems)
	if err != nil {
		return fmt.Errorf("marshal sealed %q array: %w", arrayKey, err)
	}
	obj[arrayKey] = reencoded
	return nil
}
