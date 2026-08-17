/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// unknownToolCallTurn renders the SSE events for an assistant turn that calls
// a tool the executor has no handler for.
func unknownToolCallTurn(msgID, callID, toolName string) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, callID, toolName),
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
}

// TestUnknownToolCallWrapsSentinel proves the executor wraps
// agenttrace.ErrUnknownTool into the error it records for a hallucinated tool
// name, so consumers can single the class out with errors.Is instead of
// matching the model-controlled name in the message. The no-errors eval
// scorer relies on this to keep failing an unknown tool call while every
// other tool-call error is only reported.
func TestUnknownToolCallWrapsSentinel(t *testing.T) {
	var mu sync.Mutex
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()

		events := unknownToolCallTurn("msg_01", "toolu_u1", "read_the_logs")
		if n > 1 {
			events = submitCallTurn(t, "msg_02", "toolu_s1", submitInput("done"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, err := newSubmitExecutor(t, srv).Execute(ctx, errCapRequest{}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	var got error
	for _, tc := range tracer.traces[0].ToolCalls {
		if tc.Name == "read_the_logs" {
			got = tc.Error
		}
	}
	if got == nil {
		t.Fatalf("unknown tool call error: got = nil, want = non-nil (tool calls: %v)", tracer.traces[0].ToolCalls)
	}
	if !errors.Is(got, agenttrace.ErrUnknownTool) {
		t.Errorf("errors.Is(%v, agenttrace.ErrUnknownTool): got = false, want = true", got)
	}
}

// unknownToolAvailableTools pulls available_tools out of the unknown-tool
// tool_result carried by a captured Anthropic request body. It reads the
// transcript the model actually sees, so a name that only appears in the
// request's tool definitions cannot pass the assertion.
func unknownToolAvailableTools(t *testing.T, body []byte) []string {
	t.Helper()

	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	for _, m := range req.Messages {
		var blocks []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			for _, c := range b.Content {
				var payload struct {
					Error          string   `json:"error"`
					AvailableTools []string `json:"available_tools"`
				}
				if err := json.Unmarshal([]byte(c.Text), &payload); err != nil || payload.Error == "" {
					continue
				}
				return payload.AvailableTools
			}
		}
	}

	t.Fatalf("no unknown-tool tool_result in request: %s", body)
	return nil
}

// TestUnknownToolResultListsAvailableTools proves the unknown-tool result names
// the tools the model can call, so a misspelled or invented name is correctable
// on the next turn. The list must cover the registered tools and the held-out
// submit tool, which dispatches outside the tool map.
func TestUnknownToolResultListsAvailableTools(t *testing.T) {
	var mu sync.Mutex
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()

		events := unknownToolCallTurn("msg_01", "toolu_u1", "read_the_logs")
		if n > 1 {
			events = submitCallTurn(t, "msg_02", "toolu_s1", submitInput("done"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	tools := map[string]claudetool.Metadata[errCapResponse]{
		"count_lines": {
			Definition: anthropic.ToolParam{Name: "count_lines"},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			},
		},
	}

	if _, err := newSubmitExecutor(t, srv).Execute(t.Context(), errCapRequest{}, tools); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(requests); got < 2 {
		t.Fatalf("API requests: got = %d, want >= 2", got)
	}

	available := unknownToolAvailableTools(t, requests[1])
	for _, want := range []string{"count_lines", "submit_result"} {
		if !slices.Contains(available, want) {
			t.Errorf("available_tools: got = %v, want to contain %q", available, want)
		}
	}
}
