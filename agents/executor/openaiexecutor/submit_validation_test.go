/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

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

	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// submitCompletionJSON renders a completion whose single tool call invokes
// submit_result with a {reasoning, result:{answer}} payload.
func submitCompletionJSON(t *testing.T, id, callID, answer string) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"reasoning": "complete",
		"result":    map[string]any{"answer": answer},
	})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	argsLiteral, err := json.Marshal(string(args))
	if err != nil {
		t.Fatalf("marshal arguments literal: %v", err)
	}
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
        {"id": %q, "type": "function", "function": {"name": "submit_result", "arguments": %s}}
      ]
    }
  }],
  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
}`, id, callID, argsLiteral)
}

// TestSubmitRejectedByValidatorKeepsLoopGoing drives a run whose first
// submission fails validation: the executor must return the findings as the
// tool result (visible in the next request) and keep the loop alive until a
// submission passes.
func TestSubmitRejectedByValidatorKeepsLoopGoing(t *testing.T) {
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
			_, _ = io.WriteString(w, submitCompletionJSON(t, "chatcmpl-1", "call_s1", "wrong"))
			return
		}
		_, _ = io.WriteString(w, submitCompletionJSON(t, "chatcmpl-2", "call_s2", "correct"))
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
		openaiexecutor.WithResultValidator[errCapRequest, errCapResponse](func(_ context.Context, r errCapResponse, _ string) ([]callbacks.Finding, error) {
			if r.Answer != "correct" {
				return []callbacks.Finding{{
					Kind:       callbacks.FindingKindReview,
					Identifier: "wrong-answer",
					Details:    "the answer is not correct",
				}}, nil
			}
			return nil, nil
		}),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "correct"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got, want := len(requests), 2; got != want {
		t.Fatalf("API requests: got = %d, want = %d", got, want)
	}

	second := string(requests[1])
	for _, want := range []string{"findings", "wrong-answer", "Result rejected"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request missing %q in rejection tool result:\n%s", want, second)
		}
	}
}
