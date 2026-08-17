/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

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
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// unknownToolCompletionJSON renders a completion whose single tool call names
// a tool the executor has no handler for.
func unknownToolCompletionJSON(id, callID, toolName string) string {
	return fmt.Sprintf(`{
  "id": %q,
  "object": "chat.completion",
  "created": 1,
  "model": "test-model",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "content": "",
      "tool_calls": [
        {"id": %q, "type": "function", "function": {"name": %q, "arguments": "{}"}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`, id, callID, toolName)
}

// TestUnknownToolCallWrapsSentinel proves the executor wraps
// agenttrace.ErrUnknownTool into the error it records for a hallucinated tool
// name, so consumers can single the class out with errors.Is instead of
// matching the model-controlled name in the message. The wiring is duplicated
// across executors, so each one carries its own guard.
func TestUnknownToolCallWrapsSentinel(t *testing.T) {
	var mu sync.Mutex
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, unknownToolCompletionJSON("chatcmpl-1", "call_u1", "read_the_logs"))
			return
		}
		_, _ = io.WriteString(w, submitCompletionJSON(t, "chatcmpl-2", "call_s1", "done"))
	}))
	t.Cleanup(srv.Close)

	client := openai.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.OpenAIToolForResponse[errCapResponse]),
		openaiexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	if _, err := exec.Execute(ctx, errCapRequest{}, nil); err != nil {
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

// unknownToolAvailableTools pulls available_tools out of the unknown-tool tool
// message carried by a captured request body. It reads the transcript the model
// actually sees, so a name that only appears in the request's tool definitions
// cannot pass the assertion.
func unknownToolAvailableTools(t *testing.T, body []byte) []string {
	t.Helper()

	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			var parts []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Content, &parts); err != nil || len(parts) == 0 {
				continue
			}
			text = parts[0].Text
		}
		var payload struct {
			Error          string   `json:"error"`
			AvailableTools []string `json:"available_tools"`
		}
		if err := json.Unmarshal([]byte(text), &payload); err != nil || payload.Error == "" {
			continue
		}
		return payload.AvailableTools
	}

	t.Fatalf("no unknown-tool tool message in request: %s", body)
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

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, unknownToolCompletionJSON("chatcmpl-1", "call_u1", "read_the_logs"))
			return
		}
		_, _ = io.WriteString(w, submitCompletionJSON(t, "chatcmpl-2", "call_s1", "done"))
	}))
	t.Cleanup(srv.Close)

	client := openai.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.OpenAIToolForResponse[errCapResponse]),
		openaiexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]openaistool.Metadata[errCapResponse]{
		"count_lines": openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: "count_lines", Description: "count lines"},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": true}
			},
		}),
	}

	if _, err := exec.Execute(t.Context(), errCapRequest{}, tools); err != nil {
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
