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

type fieldTransform func(obj map[string]json.RawMessage, key string) error

type fieldTransforms struct {
	jsonValue   fieldTransform
	stringValue fieldTransform
	reasoning   fieldTransform
}

type objectArrayFields struct {
	key        string
	jsonKeys   []string
	stringKeys []string
}

func omitSensitiveTraceFields(raw []byte) ([]byte, error) {
	return transformSensitiveTraceFields(raw, "without payloads", fieldTransforms{
		jsonValue:   omitField,
		stringValue: omitField,
		reasoning:   omitField,
	})
}

func omitSensitiveSpanFields(raw []byte) ([]byte, error) {
	return transformSensitiveSpanFields(raw, "without payloads", omitField)
}

func omitField(obj map[string]json.RawMessage, key string) error {
	delete(obj, key)
	return nil
}

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
// turns[].provider, turns[].system, turns[].logical_model, and turns[].protocol
// are likewise left plaintext: they are low-cardinality route identifiers
// (see Trace.BeginTurnWithAttribution), not LLM prompts, and carry no
// submission-derived text.
//
// One sealing session is used for the whole event so every field shares a single
// KMS-wrapped DEK (one KMS call per event). An error is returned rather than
// falling back to plaintext — callers must fail closed and drop the event.
func sealSensitiveTraceFields(ctx context.Context, enc *payloadcrypt.Encryptor, raw []byte) ([]byte, error) {
	sess, err := enc.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("new seal session: %w", err)
	}

	return transformSensitiveTraceFields(raw, "sealed", sealTraceFieldTransforms(sess))
}

func sealTraceFieldTransforms(sess *payloadcrypt.Session) fieldTransforms {
	nested := fieldTransforms{
		jsonValue: func(obj map[string]json.RawMessage, key string) error {
			return sealJSONField(sess, obj, key)
		},
		stringValue: func(obj map[string]json.RawMessage, key string) error {
			return sealStringField(sess, obj, key)
		},
	}

	return fieldTransforms{
		jsonValue:   nested.jsonValue,
		stringValue: nested.stringValue,
		reasoning:   reasoningFieldTransform(nested),
	}
}

func reasoningFieldTransform(nested fieldTransforms) fieldTransform {
	return func(obj map[string]json.RawMessage, key string) error {
		return transformObjectArrayFields(obj, objectArrayFields{
			key:        key,
			stringKeys: []string{"thinking"},
		}, nested)
	}
}

// sealSensitiveSpanFields seals the per-turn payload fields of a marshalled
// span-event JSON (prompt_messages, completion), both JSON columns. The span's
// `metadata` is structural-only by the same contract as the trace-level metadata
// (see sealSensitiveTraceFields) and is intentionally left plaintext — its
// route-attribution keys are identifiers (see Trace.BeginTurnWithAttribution),
// not LLM prompts, so they carry no submission-derived free text.
func sealSensitiveSpanFields(ctx context.Context, enc *payloadcrypt.Encryptor, raw []byte) ([]byte, error) {
	sess, err := enc.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("new seal session: %w", err)
	}

	return transformSensitiveSpanFields(raw, "sealed", func(obj map[string]json.RawMessage, key string) error {
		return sealJSONField(sess, obj, key)
	})
}

func transformSensitiveTraceFields(raw []byte, action string, transforms fieldTransforms) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal trace event: %w", err)
	}

	if err := transforms.stringValue(obj, "input_prompt"); err != nil {
		return nil, err
	}
	if err := transforms.jsonValue(obj, "result"); err != nil {
		return nil, err
	}
	if err := transformObjectArrayFields(obj, objectArrayFields{
		key:      "tool_calls",
		jsonKeys: []string{"params", "result"},
	}, transforms); err != nil {
		return nil, err
	}
	if err := transforms.reasoning(obj, "reasoning"); err != nil {
		return nil, err
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal %s trace event: %w", action, err)
	}
	return out, nil
}

func transformSensitiveSpanFields(raw []byte, action string, jsonField fieldTransform) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal span event: %w", err)
	}

	if err := jsonField(obj, "prompt_messages"); err != nil {
		return nil, err
	}
	if err := jsonField(obj, "completion"); err != nil {
		return nil, err
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal %s span event: %w", action, err)
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

func transformObjectArrayFields(
	obj map[string]json.RawMessage,
	fields objectArrayFields,
	transforms fieldTransforms,
) error {
	v, ok := obj[fields.key]
	if isNullOrAbsent(v, ok) {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(v, &elems); err != nil {
		return fmt.Errorf("unmarshal %q array: %w", fields.key, err)
	}
	for i, elem := range elems {
		var em map[string]json.RawMessage
		if err := json.Unmarshal(elem, &em); err != nil {
			return fmt.Errorf("unmarshal %q[%d]: %w", fields.key, i, err)
		}
		for _, k := range fields.jsonKeys {
			if err := transforms.jsonValue(em, k); err != nil {
				return fmt.Errorf("%q[%d]: %w", fields.key, i, err)
			}
		}
		for _, k := range fields.stringKeys {
			if err := transforms.stringValue(em, k); err != nil {
				return fmt.Errorf("%q[%d]: %w", fields.key, i, err)
			}
		}
		reencoded, err := json.Marshal(em)
		if err != nil {
			return fmt.Errorf("marshal transformed %q[%d]: %w", fields.key, i, err)
		}
		elems[i] = reencoded
	}
	reencoded, err := json.Marshal(elems)
	if err != nil {
		return fmt.Errorf("marshal transformed %q array: %w", fields.key, err)
	}
	obj[fields.key] = reencoded
	return nil
}
