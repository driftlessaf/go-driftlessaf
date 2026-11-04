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
	"slices"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// submitCheckAndWriteTurnJSON calls submit_result, check and write, in that
// order, in one turn.
const submitCheckAndWriteTurnJSON = `{
	"candidates":[{"content":{"parts":[
		{"functionCall":{"id":"call_submit","name":"submit_result","args":{"answer":"done"}}},
		{"functionCall":{"id":"call_check","name":"check","args":{}}},
		{"functionCall":{"id":"call_write","name":"write","args":{}}}
	]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
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
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithToolCallConcurrency[errCapRequest, errCapResponse](1),
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_result"},
				Handler: func(_ context.Context, call *genai.FunctionCall, _ *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
					answer, _ := call.Args["answer"].(string)
					return toolcall.SubmitOutcome[errCapResponse]{Accepted: true, Response: errCapResponse{Answer: answer}, ToolResult: map[string]any{"success": true}}
				},
			}, nil
		}),
		googleexecutor.WithResultValidator[errCapRequest, errCapResponse](func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
			record("submit")
			return nil, nil
		}),
		googleexecutor.WithoutCacheControl[errCapRequest, errCapResponse](),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tool := func(name string, after bool) googletool.Metadata[errCapResponse] {
		return googletool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: name, AfterSiblings: after},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				record(name)
				return map[string]any{"ok": true}
			},
		})
	}
	tools := map[string]googletool.Metadata[errCapResponse]{"check": tool("check", true), "write": tool("write", false)}

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
