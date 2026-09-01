/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
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

type responseToolCall struct {
	id        string
	name      string
	arguments string
}

func toolCallCompletionJSON(t *testing.T, id string, calls ...responseToolCall) string {
	t.Helper()
	toolCalls := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"id":   call.id,
			"type": "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments,
			},
		})
	}
	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": 1,
		"model":   "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	}
	b, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	return string(b)
}

func emptyCompletionJSON(t *testing.T, finishReason string) string {
	t.Helper()
	completion := map[string]any{
		"id":      "chatcmpl-empty",
		"object":  "chat.completion",
		"created": 1,
		"model":   "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": finishReason,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 0,
			"total_tokens":      1,
		},
	}
	b, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	return string(b)
}

func newResponseRecoveryExecutor(t *testing.T, url, promptText string, maxTurns int) openaiexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	client := openai.NewClient(
		option.WithBaseURL(url),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt(`{{prompt}}`)
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	prompt, err = prompt.BindJSON("prompt", promptText)
	if err != nil {
		t.Fatalf("BindJSON: %v", err)
	}
	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		openaiexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.OpenAIToolForResponse[errCapResponse]),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

// TestMalformedToolArgumentsAreRetriedWithoutTranscriptReplay reproduces the
// Baseten failure mode: the gateway returns malformed tool arguments once, but
// rejects them if the client replays that assistant message on the next turn.
func TestMalformedToolArgumentsAreRetriedWithoutTranscriptReplay(t *testing.T) {
	const promptText = "review this change"
	const malformedArguments = `{"reasoning":"truncated`
	var requests atomic.Int32
	retryRequest := make(chan []byte, 1)
	srv := newValidatingOpenAIServer(t, func(reqNum int, body []byte) string {
		requests.Store(int32(reqNum))
		if reqNum == 1 {
			return toolCallCompletionJSON(t, "chatcmpl-bad", responseToolCall{
				id:        "call_bad",
				name:      "submit_result",
				arguments: malformedArguments,
			})
		}
		retryRequest <- append([]byte(nil), body...)
		return submitCompletionJSON(t, "chatcmpl-good", "call_good", "recovered")
	})

	exec := newResponseRecoveryExecutor(t, srv.URL, promptText, 5)
	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)
	resp, err := exec.Execute(ctx, errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "recovered"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got, want := requests.Load(), int32(2); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}

	second := string(<-retryRequest)
	if strings.Contains(second, "call_bad") || strings.Contains(second, malformedArguments) {
		t.Errorf("retry request replayed malformed assistant tool call:\n%s", second)
	}
	for _, want := range []string{promptText, "invalid JSON arguments", "no tools were executed"} {
		if !strings.Contains(second, want) {
			t.Errorf("retry request missing %q:\n%s", want, second)
		}
	}

	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	var rejected *agenttrace.ToolCall[errCapResponse]
	for _, call := range tracer.traces[0].ToolCalls {
		if call.ID == "call_bad" {
			rejected = call
			break
		}
	}
	if rejected == nil {
		t.Fatal("trace omitted rejected malformed tool call")
	}
	if !rejected.Recoverable {
		t.Error("malformed tool call: got recoverable = false, want true")
	}
}

// TestMalformedToolCallRejectsWholeBatch proves a malformed sibling cannot
// allow a valid side-effecting tool to run before the turn is retried.
func TestMalformedToolCallRejectsWholeBatch(t *testing.T) {
	var calls atomic.Int32
	var requests atomic.Int32
	retryRequest := make(chan []byte, 1)
	srv := newValidatingOpenAIServer(t, func(reqNum int, body []byte) string {
		requests.Store(int32(reqNum))
		if reqNum == 1 {
			return toolCallCompletionJSON(t, "chatcmpl-batch",
				responseToolCall{id: "call_side_effect", name: "side_effect", arguments: `{}`},
				responseToolCall{id: "call_bad", name: "submit_result", arguments: `{"reasoning":`},
			)
		}
		retryRequest <- append([]byte(nil), body...)
		return submitCompletionJSON(t, "chatcmpl-good", "call_good", "recovered")
	})

	exec := newResponseRecoveryExecutor(t, srv.URL, "go", 5)
	tools := map[string]openaistool.Metadata[errCapResponse]{
		"side_effect": openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: "side_effect", Description: "records a side effect"},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				calls.Add(1)
				return map[string]any{"ok": true}
			},
		}),
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "recovered"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("side-effecting handler calls: got = %d, want = 0", got)
	}
	if got, want := requests.Load(), int32(2); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}
	second := string(<-retryRequest)
	if strings.Contains(second, "call_side_effect") || strings.Contains(second, "call_bad") {
		t.Errorf("retry request replayed rejected tool-call batch:\n%s", second)
	}
}

// TestEmptyResponseAfterToolCallIsRetried reproduces the GPT OSS red-team
// failure: after a valid tool result, one natural-stop response contains no
// text and no tool calls. The executor must preserve the valid transcript and
// nudge the model instead of ending the run.
func TestEmptyResponseAfterToolCallIsRetried(t *testing.T) {
	var requests atomic.Int32
	var toolCalls atomic.Int32
	retryRequest := make(chan []byte, 1)
	srv := newValidatingOpenAIServer(t, func(reqNum int, body []byte) string {
		requests.Store(int32(reqNum))
		switch reqNum {
		case 1:
			return toolCallCompletionJSON(t, "chatcmpl-search", responseToolCall{
				id:        "call_search",
				name:      "search_skills",
				arguments: `{"query":"worker run"}`,
			})
		case 2:
			return emptyCompletionJSON(t, "stop")
		default:
			retryRequest <- append([]byte(nil), body...)
			return submitCompletionJSON(t, "chatcmpl-good", "call_good", "recovered")
		}
	})

	exec := newResponseRecoveryExecutor(t, srv.URL, "review safely", 10)
	tools := map[string]openaistool.Metadata[errCapResponse]{
		"search_skills": openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: "search_skills", Description: "search available skills"},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				toolCalls.Add(1)
				return map[string]any{"skills": []string{}}
			},
		}),
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "recovered"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got, want := toolCalls.Load(), int32(1); got != want {
		t.Errorf("search_skills calls: got = %d, want = %d", got, want)
	}
	if got, want := requests.Load(), int32(3); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}
	third := string(<-retryRequest)
	for _, want := range []string{"call_search", "last response was empty"} {
		if !strings.Contains(third, want) {
			t.Errorf("retry request missing %q:\n%s", want, third)
		}
	}
}

func TestEmptyResponseRetryIsBounded(t *testing.T) {
	var requests atomic.Int32
	srv := newValidatingOpenAIServer(t, func(reqNum int, _ []byte) string {
		requests.Store(int32(reqNum))
		return emptyCompletionJSON(t, "stop")
	})

	exec := newResponseRecoveryExecutor(t, srv.URL, "go", 100)
	_, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err == nil {
		t.Fatal("Execute: got nil error, want failure after repeated empty responses")
	}
	if !strings.Contains(err.Error(), "unusable response repeatedly") {
		t.Errorf("error: got = %q, want bounded unusable-response failure", err)
	}
	if strings.Contains(err.Error(), "maximum conversation turns") {
		t.Errorf("error: got = %q, want specific invalid-response error", err)
	}
	if got, want := requests.Load(), int32(4); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}
}

func TestEmptyResponseWithTerminalFinishReasonIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	srv := newValidatingOpenAIServer(t, func(reqNum int, _ []byte) string {
		requests.Store(int32(reqNum))
		return emptyCompletionJSON(t, "content_filter")
	})

	exec := newResponseRecoveryExecutor(t, srv.URL, "go", 10)
	_, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err == nil {
		t.Fatal("Execute: got nil error, want terminal content-filter failure")
	}
	if !strings.Contains(err.Error(), `finish_reason="content_filter"`) {
		t.Errorf("error: got = %q, want finish reason", err)
	}
	if got, want := requests.Load(), int32(1); got != want {
		t.Errorf("API requests: got = %d, want = %d", got, want)
	}
}
