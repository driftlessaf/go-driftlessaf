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
	"sync/atomic"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// slowToolAndSubmitTurnJSON is a generateContent response for an assistant
// turn that calls slow_tool and submit_result in parallel (two functionCall
// parts).
const slowToolAndSubmitTurnJSON = `{
	"candidates":[{"content":{"parts":[
		{"functionCall":{"id":"call_slow","name":"slow_tool","args":{}}},
		{"functionCall":{"id":"call_submit","name":"submit_result","args":{"answer":"done"}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
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
		googleexecutor.WithResultValidator[errCapRequest, errCapResponse](func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
			close(validatorStarted)
			validatorSawDone.Store(handlerDone.Load())
			return nil, nil
		}),
		// Disable context caching so we don't need to fake the Caches.Create
		// endpoint — orthogonal to the quiesce ordering under test.
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := map[string]googletool.Metadata[errCapResponse]{
		"slow_tool": {
			Definition: &genai.FunctionDeclaration{Name: "slow_tool"},
			Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse], _ *errCapResponse) *genai.FunctionResponse {
				select {
				case <-validatorStarted:
					validatorRanEarly.Store(true)
				case <-time.After(250 * time.Millisecond):
				}
				handlerDone.Store(true)
				return &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: map[string]any{"ok": true}}
			},
		},
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
