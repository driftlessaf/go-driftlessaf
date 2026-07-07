/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// sseBody renders raw JSON events as a text/event-stream response body in
// the shape the Anthropic streaming API produces.
func sseBody(t *testing.T, events []string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		var typed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(e), &typed); err != nil {
			t.Fatalf("sseBody: invalid event JSON %q: %v", e, err)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typed.Type, e)
	}
	return b.String()
}

// TestEmptyTextBlockNotReplayed is the regression test for the fleet-wide
// "messages: text content blocks must be non-empty" 400s: during provider
// anomaly windows the model streams a degenerate empty text block (opened and
// closed with zero text_delta events) alongside a real tool_use block, and
// replaying the accumulated assistant message verbatim made the API reject
// the next request with a non-retryable 400 on the conversation's final turn.
// The executor must strip the empty block before the replay so the next
// request carries no empty text blocks and the loop completes.
func TestEmptyTextBlockNotReplayed(t *testing.T) {
	// Turn 1: the provider-anomaly shape — an empty text block alongside a
	// real tool call, with the 1-token usage signature observed in production.
	firstTurn := []string{
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_replay","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"check\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
	// Turn 2: a normal final answer the fallback JSON parser accepts.
	secondTurn := []string{
		`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"42\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}

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

		events := firstTurn
		if n > 1 {
			events = secondTurn
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]claudetool.Metadata[errCapResponse]{
		"lookup": {
			Definition: anthropic.ToolParam{
				Name:        "lookup",
				Description: anthropic.String("Look something up."),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type: "object",
					Properties: map[string]any{
						"reasoning": map[string]any{"type": "string"},
					},
				},
			},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"result": "ok"}
			},
		},
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := len(requests); got < 2 {
		t.Fatalf("HTTP requests: got = %d, want >= 2", got)
	}

	// The second request replays the accumulated assistant message. It must
	// carry the tool_use block but no empty text block — that exact shape is
	// what the API rejects with the non-retryable 400.
	var second struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				ID   string `json:"id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(requests[1], &second); err != nil {
		t.Fatalf("unmarshal second request: %v", err)
	}

	var sawToolUse bool
	for _, m := range second.Messages {
		for _, cb := range m.Content {
			if cb.Type == "text" && strings.TrimSpace(cb.Text) == "" {
				t.Errorf("second request replays an empty text block in a %s message", m.Role)
			}
			if cb.Type == "tool_use" && cb.ID == "toolu_replay" {
				sawToolUse = true
			}
		}
	}
	if !sawToolUse {
		t.Error("second request is missing the tool_use block: normalization stripped real content")
	}
}
