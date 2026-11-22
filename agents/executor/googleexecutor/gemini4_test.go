/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// newLookupThenSubmitExecutor builds an executor with a system prompt, so
// each Execute call first gets or creates a context cache.
func newLookupThenSubmitExecutor(t *testing.T, client *genai.Client, model string) googleexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	sysPrompt, err := promptbuilder.NewPrompt("system")
	if err != nil {
		t.Fatalf("NewPrompt (system): %v", err)
	}
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		googleexecutor.WithModel[errCapRequest, errCapResponse](model),
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		googleexecutor.WithSystemInstructions[errCapRequest, errCapResponse](sysPrompt),
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

// executeLookupThenSubmit runs one two-request conversation: a lookup tool
// turn, then a submit turn.
func executeLookupThenSubmit(t *testing.T, exec googleexecutor.Interface[errCapRequest, errCapResponse]) {
	t.Helper()
	tools := map[string]googletool.Metadata[errCapResponse]{
		"lookup": {
			Definition: &genai.FunctionDeclaration{Name: "lookup"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"ok": true}}
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
}

// TestGemini4SkipsExplicitContextCache checks that the executor never asks a
// model that rejects explicit context caches to create one, and sends the
// system instruction inline instead.
func TestGemini4SkipsExplicitContextCache(t *testing.T) {
	var cacheReqs int
	var mu sync.Mutex
	var inlineSystem []bool
	srv := newValidatingGenerateContentServer(t, &cacheReqs, func(reqNum int, body []byte) string {
		mu.Lock()
		inlineSystem = append(inlineSystem, strings.Contains(string(body), `"systemInstruction"`))
		mu.Unlock()
		if reqNum%2 == 1 {
			return lookupTurnJSON
		}
		return submitTurnJSON
	})

	exec := newLookupThenSubmitExecutor(t, newTestClient(t, srv.URL), "gemini-4-argon")
	for range 2 {
		executeLookupThenSubmit(t, exec)
	}

	if cacheReqs != 0 {
		t.Errorf("cachedContents create calls: got = %d, want = 0", cacheReqs)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := len(inlineSystem), 4; got != want {
		t.Fatalf("generateContent requests: got = %d, want = %d", got, want)
	}
	for i, inline := range inlineSystem {
		if !inline {
			t.Errorf("request %d: system instruction not sent inline", i+1)
		}
	}
}

// TestCacheCreateFailure checks that a cache create the provider rejects with
// a 4xx is held for the cache TTL instead of repeated by the next Execute
// call, while a transient failure is retried by the next Execute call.
func TestCacheCreateFailure(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCacheReqs int
	}{{
		name:          "rejection holds creation",
		status:        http.StatusBadRequest,
		body:          `{"error":{"code":400,"message":"Model does not support explicit context caching.","status":"INVALID_ARGUMENT"}}`,
		wantCacheReqs: 1,
	}, {
		name:          "transient failure retries",
		status:        http.StatusServiceUnavailable,
		body:          `{"error":{"code":503,"message":"The service is currently unavailable.","status":"UNAVAILABLE"}}`,
		wantCacheReqs: 2,
	}}
	const runs = 2
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var cacheReqs, genReqs int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				defer mu.Unlock()
				if strings.Contains(r.URL.Path, "cachedContents") {
					cacheReqs++
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
					return
				}
				genReqs++
				if genReqs%2 == 1 {
					_, _ = io.WriteString(w, lookupTurnJSON)
					return
				}
				_, _ = io.WriteString(w, submitTurnJSON)
			}))
			t.Cleanup(srv.Close)

			exec := newLookupThenSubmitExecutor(t, newTestClient(t, srv.URL), "gemini-3.8-flash")
			for range runs {
				executeLookupThenSubmit(t, exec)
			}

			mu.Lock()
			defer mu.Unlock()
			if got, want := genReqs, 2*runs; got != want {
				t.Fatalf("generateContent requests: got = %d, want = %d", got, want)
			}
			if got := cacheReqs; got != tt.wantCacheReqs {
				t.Errorf("cachedContents create calls: got = %d, want = %d", got, tt.wantCacheReqs)
			}
		})
	}
}

// TestExecuteMergesLongDecode checks that a generation the provider splits
// across requests reaches the caller whole: the SDK sends the continuation
// token back, and the executor parses the text of every request, not only the
// last one.
func TestExecuteMergesLongDecode(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"answer\": \"4"}]},"finishReason":"CONTINUATION","continuationToken":"Y29udGludWUtMQ=="}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":65536,"totalTokenCount":65546}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"2\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`)
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithModel[errCapRequest, errCapResponse]("gemini-4-argon"),
		googleexecutor.WithMaxOutputTokens[errCapRequest, errCapResponse](200000),
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(bodies), 2; got != want {
		t.Fatalf("generateContent requests: got = %d, want = %d", got, want)
	}
	if strings.Contains(bodies[0], "continuationToken") {
		t.Errorf("request 1 carries a continuation token: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"continuationToken":"Y29udGludWUtMQ=="`) {
		t.Errorf("request 2 does not send back the continuation token: %s", bodies[1])
	}
}
