/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
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
