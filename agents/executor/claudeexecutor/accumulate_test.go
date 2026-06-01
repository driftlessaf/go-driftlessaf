/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// unmarshalEvent constructs a MessageStreamEventUnion from raw JSON,
// mirroring how the SDK's streaming decoder populates events. This
// avoids reaching into unexported JSON fields.
func unmarshalEvent(t *testing.T, raw string) anthropic.MessageStreamEventUnion {
	t.Helper()
	var event anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshalEvent: %v\nraw: %s", err, raw)
	}
	return event
}

// TestEmptyRawMessageMarshalFailure is the RED proof: demonstrates that
// json.Marshal fails on a Message containing a tool_use block with a non-nil
// zero-length json.RawMessage Input. This is the exact failure observed in
// production when the SDK's Accumulate re-marshals the message.
func TestEmptyRawMessageMarshalFailure(t *testing.T) {
	t.Parallel()

	msg := anthropic.Message{
		Content: []anthropic.ContentBlockUnion{
			{Type: "tool_use", ID: "toolu_01", Name: "read_file", Input: json.RawMessage{}},
		},
	}

	// Without the fix, this marshal fails with exactly:
	// "json: error calling MarshalJSON for type json.RawMessage: unexpected end of JSON input"
	_, err := json.Marshal(&msg)
	if err == nil {
		t.Fatal("expected marshal to fail on non-nil zero-length json.RawMessage, but it succeeded")
	}
	if !isEmptyRawMessageMarshalErr(err) {
		t.Fatalf("unexpected error type: %v", err)
	}

	// Apply the fix
	if !normalizeEmptyToolInputs(&msg) {
		t.Fatal("normalizeEmptyToolInputs should have changed the empty input")
	}

	// After the fix, marshal succeeds
	_, err = json.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal should succeed after normalize, got: %v", err)
	}

	if got := string(msg.Content[0].Input); got != "{}" {
		t.Errorf("Input after normalize = %q, want %q", got, "{}")
	}
}

// TestAccumulateWithNormalize exercises the accumulate-and-repair loop using
// real SDK streaming event sequences. The repair-and-continue pattern mirrors
// the production code path in executor.go.
func TestAccumulateWithNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		events     []string // raw JSON for each streaming event
		wantErr    bool
		wantInputs map[string]string // tool name -> expected Input JSON
	}{
		{
			name: "tool_use with empty input accumulates without error",
			events: []string{
				`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read_file","input":{}}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
				`{"type":"message_stop"}`,
			},
			wantErr: false,
			wantInputs: map[string]string{
				"read_file": "{}",
			},
		},
		{
			name: "tool_use with real input is preserved byte-for-byte",
			events: []string{
				`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"read_file","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/etc/passwd\","}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"limit\":100}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
				`{"type":"message_stop"}`,
			},
			wantErr: false,
			wantInputs: map[string]string{
				"read_file": `{"path":"/etc/passwd","limit":100}`,
			},
		},
		{
			name: "mixed content: text then tool_use with empty input",
			events: []string{
				`{"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"analyzing"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_03","name":"list_dir","input":{}}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
				`{"type":"message_stop"}`,
			},
			wantErr: false,
			wantInputs: map[string]string{
				"list_dir": "{}",
			},
		},
		{
			name: "multiple tool_use blocks: one empty one with real input",
			events: []string{
				`{"type":"message_start","message":{"id":"msg_04","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_04a","name":"get_status","input":{}}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_04b","name":"read_file","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/var/log/syslog\"}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}`,
				`{"type":"message_stop"}`,
			},
			wantErr: false,
			wantInputs: map[string]string{
				"get_status": "{}",
				"read_file":  `{"path":"/var/log/syslog"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events := make([]anthropic.MessageStreamEventUnion, len(tt.events))
			for i, raw := range tt.events {
				events[i] = unmarshalEvent(t, raw)
			}

			// Replicate the accumulate loop with the repair-and-continue fix
			var msg anthropic.Message
			var accErr error
			for _, event := range events {
				if err := msg.Accumulate(event); err != nil {
					if isEmptyRawMessageMarshalErr(err) && normalizeEmptyToolInputs(&msg) {
						continue
					}
					accErr = err
					break
				}
			}

			if (accErr != nil) != tt.wantErr {
				t.Fatalf("accumulate error = %v, wantErr %v", accErr, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			// Verify tool inputs match expectations
			for _, cb := range msg.Content {
				if cb.Type != "tool_use" {
					continue
				}
				want, ok := tt.wantInputs[cb.Name]
				if !ok {
					continue
				}
				got := string(cb.Input)
				if got != want {
					t.Errorf("tool %q Input = %q, want %q", cb.Name, got, want)
				}
				delete(tt.wantInputs, cb.Name)
			}

			for name, want := range tt.wantInputs {
				t.Errorf("expected tool %q with Input %q not found in message content", name, want)
			}
		})
	}
}

// TestNormalizeEmptyToolInputs verifies the helper only touches empty/invalid
// tool-use Input and never alters valid JSON.
func TestNormalizeEmptyToolInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   json.RawMessage
		want    string
		changed bool
	}{
		{name: "nil input", input: nil, want: "{}", changed: true},
		{name: "zero-length non-nil", input: json.RawMessage{}, want: "{}", changed: true},
		{name: "empty object already valid", input: json.RawMessage("{}"), want: "{}", changed: false},
		{name: "valid object preserved", input: json.RawMessage(`{"key":"val"}`), want: `{"key":"val"}`, changed: false},
		{name: "invalid JSON repaired", input: json.RawMessage(`{broken`), want: "{}", changed: true},
		{name: "valid array preserved", input: json.RawMessage(`[1,2,3]`), want: `[1,2,3]`, changed: false},
		{name: "text block skipped", input: json.RawMessage("{}"), want: "{}", changed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			blockType := "tool_use"
			if tt.name == "text block skipped" {
				blockType = "text"
			}

			msg := anthropic.Message{
				Content: []anthropic.ContentBlockUnion{
					{Type: blockType, Input: tt.input, Name: "test_tool", ID: "toolu_test"},
				},
			}

			got := normalizeEmptyToolInputs(&msg)
			if got != tt.changed {
				t.Errorf("normalizeEmptyToolInputs() = %v, want %v", got, tt.changed)
			}
			gotInput := string(msg.Content[0].Input)
			if gotInput != tt.want {
				t.Errorf("Input after normalize = %q, want %q", gotInput, tt.want)
			}
		})
	}
}

// TestIsEmptyRawMessageMarshalErr verifies error matching is precise.
func TestIsEmptyRawMessageMarshalErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "exact SDK marshal error", err: jsonMarshalEmptyRawMessage(), want: true},
		{name: "unrelated JSON syntax error", err: json.Unmarshal([]byte(`{"bad":}`), new(any)), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isEmptyRawMessageMarshalErr(tt.err); got != tt.want {
				t.Errorf("isEmptyRawMessageMarshalErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestNormalizeToolUseInput verifies the defense-in-depth helper used at
// tool-call consumption sites.
func TestNormalizeToolUseInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{name: "nil", input: nil, want: "{}"},
		{name: "zero-length", input: json.RawMessage{}, want: "{}"},
		{name: "valid empty object", input: json.RawMessage("{}"), want: "{}"},
		{name: "valid real input", input: json.RawMessage(`{"path":"/etc/hosts"}`), want: `{"path":"/etc/hosts"}`},
		{name: "invalid JSON", input: json.RawMessage(`{corrupt`), want: "{}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(normalizeToolUseInput(tt.input))
			if got != tt.want {
				t.Errorf("normalizeToolUseInput(%q) = %q, want %q", string(tt.input), got, tt.want)
			}
		})
	}
}

// jsonMarshalEmptyRawMessage produces the exact error that json.Marshal emits
// for a non-nil zero-length json.RawMessage -- the error the fix targets.
func jsonMarshalEmptyRawMessage() error {
	_, err := json.Marshal(struct {
		Input json.RawMessage `json:"input"`
	}{Input: json.RawMessage{}})
	return err
}
