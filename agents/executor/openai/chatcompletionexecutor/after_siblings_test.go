/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package chatcompletionexecutor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openai/chatcompletionexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// submitCheckAndWriteTurnJSON calls submit_result, check and write, in that
// order, in one turn.
const submitCheckAndWriteTurnJSON = `{
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
        {"id": "call_submit", "type": "function", "function": {"name": "submit_result", "arguments": "{}"}},
        {"id": "call_check", "type": "function", "function": {"name": "check", "arguments": "{}"}},
        {"id": "call_write", "type": "function", "function": {"name": "write", "arguments": "{}"}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`

// TestAfterSiblingsToolRunsAfterItsTurn: in a turn of submit, check and
// write, listed in that order, the check (AfterSiblings) runs after the write
// and the submission's validator after the check. With one call at a time,
// slot order would run the check first, and a check held with the submit
// would run after it.
func TestAfterSiblingsToolRunsAfterItsTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, submitCheckAndWriteTurnJSON)
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := chatcompletionexecutor.New[errCapRequest, errCapResponse](
		openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		prompt,
		chatcompletionexecutor.WithToolCallConcurrency[errCapRequest, errCapResponse](1),
		chatcompletionexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (openaistool.SubmitMetadata[errCapResponse], error) {
			return openaistool.SubmitMetadata[errCapResponse]{
				Definition: openai.ChatCompletionFunctionToolParam{Function: shared.FunctionDefinitionParam{Name: "submit_result"}},
				Handler: func(context.Context, openai.ChatCompletionMessageToolCallUnion, *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					return toolcall.SubmitOutcome[errCapResponse]{Accepted: true, Response: errCapResponse{Answer: "done"}, ToolResult: map[string]any{"success": true}}
				},
			}, nil
		}),
		chatcompletionexecutor.WithResultValidator[errCapRequest, errCapResponse](func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
			record("submit")
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tool := func(name string, after bool) openaistool.Metadata[errCapResponse] {
		return openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: name, AfterSiblings: after},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				record(name)
				return map[string]any{"ok": true}
			},
		})
	}
	tools := map[string]openaistool.Metadata[errCapResponse]{"check": tool("check", true), "write": tool("write", false)}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if want := []string{"write", "check", "submit"}; !slices.Equal(order, want) {
		t.Errorf("run order: got = %v, want = %v", order, want)
	}
}
