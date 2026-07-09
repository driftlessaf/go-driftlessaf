/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package agenttrace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace/payloadcrypt"
)

func testEncryptor(t *testing.T) *payloadcrypt.Encryptor {
	t.Helper()
	wrap := func(_ context.Context, dek []byte) ([]byte, error) {
		out := make([]byte, len(dek))
		for i, b := range dek {
			out[i] = b ^ 0x5a
		}
		return out, nil
	}
	enc, err := payloadcrypt.New("projects/p/locations/us-central1/keyRings/argos/cryptoKeys/agent-trace-payload-key", wrap)
	if err != nil {
		t.Fatalf("payloadcrypt.New: %v", err)
	}
	return enc
}

func xorUnwrap(_ string, wrapped []byte) ([]byte, error) {
	out := make([]byte, len(wrapped))
	for i, b := range wrapped {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

// openField recovers the plaintext of a sealed field. jsonString is true for
// STRING-column fields (the envelope is itself wrapped in a JSON string).
func openField(t *testing.T, v json.RawMessage, jsonString bool) []byte {
	t.Helper()
	env := []byte(v)
	if jsonString {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("field is not a JSON string: %v", err)
		}
		env = []byte(s)
	}
	pt, err := payloadcrypt.Open(env, xorUnwrap)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return pt
}

func TestSealSensitiveTraceFields(t *testing.T) {
	enc := testEncryptor(t)

	// A representative marshalled trace event: structural fields + sensitive
	// payloads, including a tool call and a reasoning block.
	raw := []byte(`{
		"id": "20260709-120000-abcd",
		"model": "claude-opus-4-8",
		"agent_name": "vuln-analyzer",
		"source": "vuln-analyzer-wq",
		"input_prompt": "analyze CVE-2025-1 in requests 2.28.0",
		"result": {"verdict": "patched"},
		"tool_calls": [
			{"id": "t1", "name": "read_file", "params": {"path": "setup.py"}, "result": "contents"}
		],
		"reasoning": [{"thinking": "the vuln is in urllib3"}],
		"turns": [{"index": 0, "input_tokens": 10, "failed": false}],
		"exec_context": {"reconciler_key": "python/requests/2.28.0"}
	}`)

	out, err := sealSensitiveTraceFields(t.Context(), enc, raw)
	if err != nil {
		t.Fatalf("sealSensitiveTraceFields: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}

	// Structural fields must be untouched.
	for k, want := range map[string]string{
		"id":         `"20260709-120000-abcd"`,
		"model":      `"claude-opus-4-8"`,
		"agent_name": `"vuln-analyzer"`,
		"source":     `"vuln-analyzer-wq"`,
	} {
		if got := strings.TrimSpace(string(obj[k])); got != want {
			t.Errorf("structural field %q changed: got %s want %s", k, got, want)
		}
	}
	// turns and exec_context must survive verbatim (needed for cost view).
	if !strings.Contains(string(obj["turns"]), `"input_tokens":10`) {
		t.Errorf("turns altered: %s", obj["turns"])
	}
	if !strings.Contains(string(obj["exec_context"]), "python/requests/2.28.0") {
		t.Errorf("exec_context altered: %s", obj["exec_context"])
	}

	// Sensitive fields must no longer contain plaintext.
	if strings.Contains(string(out), "requests 2.28.0") ||
		strings.Contains(string(out), "urllib3") ||
		strings.Contains(string(out), "setup.py") ||
		strings.Contains(string(out), "patched") {
		t.Fatalf("sealed output still contains plaintext: %s", out)
	}

	// input_prompt (STRING column) round-trips.
	if got := string(openField(t, obj["input_prompt"], true)); got != `"analyze CVE-2025-1 in requests 2.28.0"` {
		t.Errorf("input_prompt round-trip: %s", got)
	}
	// result (JSON column) round-trips.
	if got := string(openField(t, obj["result"], false)); got != `{"verdict": "patched"}` {
		t.Errorf("result round-trip: %s", got)
	}

	// tool_calls[0].params/.result seal; id/name stay.
	var tcs []map[string]json.RawMessage
	if err := json.Unmarshal(obj["tool_calls"], &tcs); err != nil {
		t.Fatalf("tool_calls: %v", err)
	}
	if string(tcs[0]["name"]) != `"read_file"` {
		t.Errorf("tool_call name changed: %s", tcs[0]["name"])
	}
	if got := string(openField(t, tcs[0]["params"], false)); got != `{"path": "setup.py"}` {
		t.Errorf("tool params round-trip: %s", got)
	}
	if got := string(openField(t, tcs[0]["result"], false)); got != `"contents"` {
		t.Errorf("tool result round-trip: %s", got)
	}

	// reasoning[0].thinking (STRING column) seals + round-trips.
	var rs []map[string]json.RawMessage
	if err := json.Unmarshal(obj["reasoning"], &rs); err != nil {
		t.Fatalf("reasoning: %v", err)
	}
	if got := string(openField(t, rs[0]["thinking"], true)); got != `"the vuln is in urllib3"` {
		t.Errorf("thinking round-trip: %s", got)
	}
}

func TestSealSensitiveSpanFields(t *testing.T) {
	enc := testEncryptor(t)
	raw := []byte(`{
		"trace_id": "20260709-120000-abcd",
		"span_id": "20260709-120000-abcd-t0",
		"model_id": "claude-opus-4-8",
		"prompt_messages": [{"role": "user", "content": "secret prompt body"}],
		"completion": {"text": "secret completion body"},
		"prompt_hash": "deadbeef",
		"token_counts": {"input": 10, "output": 20}
	}`)

	out, err := sealSensitiveSpanFields(t.Context(), enc, raw)
	if err != nil {
		t.Fatalf("sealSensitiveSpanFields: %v", err)
	}
	if strings.Contains(string(out), "secret prompt body") || strings.Contains(string(out), "secret completion body") {
		t.Fatalf("sealed span still contains plaintext: %s", out)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	// Structural fields untouched (prompt_hash + token_counts drive analytics).
	if string(obj["prompt_hash"]) != `"deadbeef"` {
		t.Errorf("prompt_hash changed: %s", obj["prompt_hash"])
	}
	if !strings.Contains(string(obj["token_counts"]), `"input":10`) {
		t.Errorf("token_counts altered: %s", obj["token_counts"])
	}
	if got := string(openField(t, obj["prompt_messages"], false)); got != `[{"role": "user", "content": "secret prompt body"}]` {
		t.Errorf("prompt_messages round-trip: %s", got)
	}
	if got := string(openField(t, obj["completion"], false)); got != `{"text": "secret completion body"}` {
		t.Errorf("completion round-trip: %s", got)
	}
}

func TestSealHandlesNullAndAbsentFields(t *testing.T) {
	enc := testEncryptor(t)
	// result is null; reasoning/tool_calls absent; input_prompt empty string.
	raw := []byte(`{"id":"x","input_prompt":"","result":null}`)
	out, err := sealSensitiveTraceFields(t.Context(), enc, raw)
	if err != nil {
		t.Fatalf("sealSensitiveTraceFields: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if string(obj["result"]) != "null" {
		t.Errorf("null result should stay null, got %s", obj["result"])
	}
	// empty-string input_prompt is non-null, so it seals; ensure it round-trips.
	if got := string(openField(t, obj["input_prompt"], true)); got != `""` {
		t.Errorf("empty input_prompt round-trip: %s", got)
	}
}
