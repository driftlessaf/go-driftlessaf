/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// unknownFunctionTurnJSON is a generateContent response for an assistant turn
// that calls a function the executor has no handler for.
const unknownFunctionTurnJSON = `{
	"candidates":[{"content":{"parts":[
		{"functionCall":{"id":"call_unknown","name":"read_the_logs","args":{}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`

// TestUnknownFunctionCallWrapsSentinel proves the executor wraps
// agenttrace.ErrUnknownTool into the error it records for a hallucinated
// function name, so consumers can single the class out with errors.Is instead
// of matching the model-controlled name in the message. The wiring is
// duplicated across executors, so each one carries its own guard.
func TestUnknownFunctionCallWrapsSentinel(t *testing.T) {
	var mu sync.Mutex
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, unknownFunctionTurnJSON)
			return
		}
		// submitTurnJSON is shared with validating_server_test.go.
		_, _ = io.WriteString(w, submitTurnJSON)
	}))
	t.Cleanup(srv.Close)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
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
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		// Context caching is orthogonal here and would need a faked
		// Caches.Create endpoint.
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
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
		t.Fatalf("unknown function call error: got = nil, want = non-nil (tool calls: %v)", tracer.traces[0].ToolCalls)
	}
	if !errors.Is(got, agenttrace.ErrUnknownTool) {
		t.Errorf("errors.Is(%v, agenttrace.ErrUnknownTool): got = false, want = true", got)
	}
}
