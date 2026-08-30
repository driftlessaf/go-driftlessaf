/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// malformedFunctionCallJSON matches the provider response shape that exposed
// the regression in live conformance: the finish reason identifies a malformed
// call, but the candidate has no valid content for the GenAI SDK to retain.
const malformedFunctionCallJSON = `{
	"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

func newMalformedRetryExecutor(t *testing.T, url string, prompt *promptbuilder.Prompt, maxRetries int) googleexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, url),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(maxRetries)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[errCapResponse]{
						Accepted:   true,
						Response:   errCapResponse{Answer: answer},
						ToolResult: map[string]any{"success": true},
					}
				},
			}, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

func newRuntimePrompt(t *testing.T, text string) *promptbuilder.Prompt {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt(`{{text}}`)
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	prompt, err = prompt.BindJSON("text", text)
	if err != nil {
		t.Fatalf("BindJSON: %v", err)
	}
	return prompt
}

func requestTexts(t *testing.T, body []byte) []string {
	t.Helper()
	var request struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("unmarshal retry request: %v", err)
	}
	var texts []string
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			texts = append(texts, part.Text)
		}
	}
	return texts
}

func TestMalformedFunctionCallRetryPreservesOriginalInput(t *testing.T) {
	originalPrompt := "submit " + rand.Text()

	retryRequest := make(chan []byte, 1)
	srv := newValidatingGenerateContentServer(t, nil, func(reqNum int, body []byte) string {
		if reqNum == 1 {
			return malformedFunctionCallJSON
		}
		if reqNum == 2 {
			retryRequest <- append([]byte(nil), body...)
		}
		return submitTurnJSON
	})

	prompt := newRuntimePrompt(t, originalPrompt)
	exec := newMalformedRetryExecutor(t, srv.URL, prompt, 0)

	resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}

	texts := requestTexts(t, <-retryRequest)
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, originalPrompt) {
		t.Errorf("retry request omitted original input: got text parts %q, want %q", texts, originalPrompt)
	}
	if !strings.Contains(joined, "function call was malformed") {
		t.Errorf("retry request omitted recovery instruction: got text parts %q", texts)
	}
}

func TestMalformedFunctionCallRetryReplaysToolResponse(t *testing.T) {
	originalPrompt := "look up " + rand.Text()

	retryRequest := make(chan []byte, 1)
	srv := newValidatingGenerateContentServer(t, nil, func(reqNum int, body []byte) string {
		switch reqNum {
		case 1:
			return lookupTurnJSON
		case 2:
			return malformedFunctionCallJSON
		case 3:
			retryRequest <- append([]byte(nil), body...)
		}
		return submitTurnJSON
	})

	prompt := newRuntimePrompt(t, originalPrompt)
	exec := newMalformedRetryExecutor(t, srv.URL, prompt, 0)
	tools := map[string]googletool.Metadata[errCapResponse]{
		"lookup": {
			Definition: &genai.FunctionDeclaration{Name: "lookup"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"answer": "found"}}
			},
		},
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}

	body := <-retryRequest
	texts := requestTexts(t, body)
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, originalPrompt) {
		t.Errorf("retry request omitted original conversation: got text parts %q, want %q", texts, originalPrompt)
	}
	if !strings.Contains(joined, "function call was malformed") {
		t.Errorf("retry request omitted recovery instruction: got text parts %q", texts)
	}
	if !strings.Contains(string(body), `"functionResponse"`) {
		t.Errorf("retry request omitted the tool response that produced the malformed turn: %s", body)
	}
}

func TestMalformedFunctionCallRetryRetriesDeadlineExceeded(t *testing.T) {
	var requests atomic.Int32
	srv := newValidatingGenerateContentServerWithStatus(t, nil, func(reqNum int, _ []byte) (int, string) {
		requests.Store(int32(reqNum))
		switch reqNum {
		case 1:
			return http.StatusOK, malformedFunctionCallJSON
		case 2:
			return http.StatusGatewayTimeout, `{"error":{"code":504,"message":"Deadline expired before operation could complete.","status":"DEADLINE_EXCEEDED"}}`
		default:
			return http.StatusOK, submitTurnJSON
		}
	})

	prompt := newRuntimePrompt(t, "submit "+rand.Text())
	exec := newMalformedRetryExecutor(t, srv.URL, prompt, 1)

	resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got, want := requests.Load(), int32(3); got != want {
		t.Errorf("generateContent requests: got = %d, want = %d", got, want)
	}
}
