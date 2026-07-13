/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// slowToolAndSubmitTurnJSON is a completion for an assistant turn that calls
// slow_tool and submit_result in parallel (two tool calls).
const slowToolAndSubmitTurnJSON = `{
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
        {"id": "call_slow", "type": "function", "function": {"name": "slow_tool", "arguments": "{}"}},
        {"id": "call_submit", "type": "function", "function": {"name": "submit_result", "arguments": "{\"answer\":\"done\"}"}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`

// TestSubmitEvaluatedAfterToolHandlers pins the quiesce guarantee documented
// on callbacks.ResultValidator: when a turn carries a submit call alongside
// other tool calls, the submission's validators run only after those
// handlers have completed — so validators that read state the handlers
// produce (worktrees, files) observe the finished state instead of racing
// them under the concurrent tool pool.
func TestSubmitEvaluatedAfterToolHandlers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, slowToolAndSubmitTurnJSON)
	}))
	t.Cleanup(srv.Close)

	// validatorStarted lets the tool handler observe a validator that began
	// while the handler was still running — the exact interleaving the
	// quiesce guarantee forbids — so regression detection rides the channel,
	// not the clock. Under the guarantee the validator never starts first,
	// the handler's wait times out, and the timeout bounds only the fixed
	// path's wall time.
	validatorStarted := make(chan struct{})
	var handlerDone, validatorRanEarly, validatorSawDone atomic.Bool

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		openai.NewClient(
			option.WithBaseURL(srv.URL),
			option.WithAPIKey("test"),
			option.WithMaxRetries(0),
		),
		prompt,
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (openaistool.SubmitMetadata[errCapResponse], error) {
			return openaistool.SubmitMetadata[errCapResponse]{
				Definition: openai.ChatCompletionToolParam{
					Function: shared.FunctionDefinitionParam{Name: "submit_result"},
				},
				Handler: func(_ context.Context, tc openai.ChatCompletionMessageToolCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					var args struct {
						Answer string `json:"answer"`
					}
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
					return toolcall.SubmitOutcome[errCapResponse]{
						Accepted:   true,
						Response:   errCapResponse{Answer: args.Answer},
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
		openaiexecutor.WithResultValidator[errCapRequest, errCapResponse](func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
			close(validatorStarted)
			validatorSawDone.Store(handlerDone.Load())
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]openaistool.Metadata[errCapResponse]{
		"slow_tool": openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: "slow_tool", Description: "test tool"},
			Handler: func(_ context.Context, _ toolcall.ToolCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) map[string]any {
				select {
				case <-validatorStarted:
					validatorRanEarly.Store(true)
				case <-time.After(250 * time.Millisecond):
				}
				handlerDone.Store(true)
				return map[string]any{"ok": true}
			},
		}),
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if validatorRanEarly.Load() {
		t.Error("validator started while the turn's other tool handler was still running")
	}
	if !validatorSawDone.Load() {
		t.Error("validator ran before the turn's other tool handlers completed")
	}
}
