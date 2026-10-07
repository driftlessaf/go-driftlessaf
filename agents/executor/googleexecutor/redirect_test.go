/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

const (
	// textOnlyTurnJSON is an assistant turn that answers in prose instead of
	// calling the submit tool, which triggers the redirect.
	textOnlyTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[{"text":"I am done."}]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`
	// widgetSubmitTurnJSON calls the renamed submit tool.
	widgetSubmitTurnJSON = `{
	"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_submit","name":"submit_widget","args":{"answer":"done"}}}]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
}`
)

// TestTextTurnRedirectNamesConfiguredSubmitTool pins the redirect that follows
// a text-only turn: it must name the submit tool the model was actually given.
// Naming the default submit_result instead tells the model to call a tool that
// does not exist, which it then reasons about turn after turn.
func TestTextTurnRedirectNamesConfiguredSubmitTool(t *testing.T) {
	var redirectBody string
	srv := newValidatingGenerateContentServer(t, nil, func(n int, body []byte) string {
		if n == 1 {
			return textOnlyTurnJSON
		}
		if n == 2 {
			redirectBody = string(body)
		}
		return widgetSubmitTurnJSON
	})

	prompt, err := promptbuilder.NewPrompt("test prompt")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, srv.URL),
		prompt,
		googleexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](func() (googletool.SubmitMetadata[errCapResponse], error) {
			return googletool.SubmitMetadata[errCapResponse]{
				Definition: &genai.FunctionDeclaration{Name: "submit_widget"},
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
	result, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
	if err != nil || result.Answer != "done" {
		t.Fatalf("Execute: result = %+v, error = %v, want the submitted answer", result, err)
	}

	if !strings.Contains(redirectBody, "call the submit_widget tool") {
		t.Errorf("redirect request: want it to name submit_widget, got body:\n%s", redirectBody)
	}
	if strings.Contains(redirectBody, "submit_result") {
		t.Errorf("redirect request: want no mention of the default submit_result, got body:\n%s", redirectBody)
	}
}
