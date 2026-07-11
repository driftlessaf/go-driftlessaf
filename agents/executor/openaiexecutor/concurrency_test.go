/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// turn1ToolCallsJSON is a completion that emits two independent tool calls in a
// single turn (parallel tool calls), in the order call_a, call_b.
const turn1ToolCallsJSON = `{
  "id": "chatcmpl-1",
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
        {"id": "call_a", "type": "function", "function": {"name": "tool_a", "arguments": "{\"reasoning\":\"r\"}"}},
        {"id": "call_b", "type": "function", "function": {"name": "tool_b", "arguments": "{\"reasoning\":\"r\"}"}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`

// turn2SubmitJSON terminates the run by calling the submit_result tool.
const turn2SubmitJSON = `{
  "id": "chatcmpl-2",
  "object": "chat.completion",
  "created": 2,
  "model": "test-model",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "content": "",
      "tool_calls": [
        {"id": "call_s", "type": "function", "function": {"name": "submit_result", "arguments": "{\"reasoning\":\"r\"}"}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`

// TestExecutorRunsTurnToolCallsConcurrently drives a turn that emits two
// independent tool calls and asserts that, under the default tool-call
// concurrency, both handlers run concurrently, their results are appended in
// the model's original order, and the terminal result is returned. Run under
// -race it also exercises concurrent access to the shared trace. This is the
// regression guard for the executor's bounded-concurrency tool dispatch.
func TestExecutorRunsTurnToolCallsConcurrently(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	var reqN atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, turn1ToolCallsJSON)
			return
		}
		// The second request carries turn 1's assistant message plus the tool
		// result messages; capture it so the test can assert their ordering.
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		_, _ = io.WriteString(w, turn2SubmitJSON)
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

	// bump records that a handler ran and tracks the maximum number of handlers
	// in flight at once. The 50ms hold guarantees an observable overlap window
	// when the dispatch is concurrent; sequential dispatch would never exceed 1.
	var ran, inFlight, maxInFlight atomic.Int32
	bump := func() {
		cur := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		ran.Add(1)
	}

	mkTool := func(name string) openaistool.Metadata[errCapResponse] {
		return openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: "test tool " + name},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
				bump()
				return map[string]any{"ok": name}
			},
		})
	}
	tools := map[string]openaistool.Metadata[errCapResponse]{
		"tool_a": mkTool("tool_a"),
		"tool_b": mkTool("tool_b"),
	}

	submit := func() (openaistool.SubmitMetadata[errCapResponse], error) {
		return openaistool.SubmitMetadata[errCapResponse]{
			Definition: openai.ChatCompletionToolParam{
				Function: shared.FunctionDefinitionParam{Name: "submit_result"},
			},
			Handler: func(context.Context, openai.ChatCompletionMessageToolCall, *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
				return toolcall.SubmitOutcome[errCapResponse]{
					Accepted:   true,
					Response:   errCapResponse{Answer: "done"},
					ToolResult: map[string]any{"success": true},
				}
			},
		}, nil
	}

	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submit),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("result.Answer: got = %q, want = %q", got, want)
	}
	if got, want := ran.Load(), int32(2); got != want {
		t.Errorf("tool handlers run: got = %d, want = %d", got, want)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Errorf("max concurrent tool handlers: got = %d, want >= 2 (handlers did not run concurrently)", got)
	}
	if got, want := reqN.Load(), int32(2); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}

	select {
	case body := <-bodyCh:
		s := string(body)
		ia, ib := strings.Index(s, "call_a"), strings.Index(s, "call_b")
		switch {
		case ia < 0 || ib < 0:
			t.Errorf("second request missing tool result ids: call_a idx = %d, call_b idx = %d", ia, ib)
		case ia > ib:
			t.Errorf("tool results out of order: call_a idx = %d, want before call_b idx = %d", ia, ib)
		}
	default:
		t.Error("second request body not captured: executor did not send a tool-results turn")
	}
}
