/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package googleexecutor_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/googletool"
	"google.golang.org/genai"
)

// emptyTurnJSON is a candidate carrying neither text nor a function call.
//
// This is what the model actually returns when it produces a degenerate turn,
// and it was the single most common non-deterministic CI failure across the
// model-backed suites, surfacing as "unexpected response format from model".
const emptyTurnJSON = `{
	"candidates":[{"content":{"parts":[]}}],
	"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":0,"totalTokenCount":1}
}`

func newEmptyResponseExecutor(t *testing.T, url, promptText string, maxTurns int) googleexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt := newRuntimePrompt(t, promptText)
	exec, err := googleexecutor.New[errCapRequest, errCapResponse](
		newTestClient(t, url),
		prompt,
		googleexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		googleexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
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

// TestEmptyResponseIsRetried is the regression test for the dominant CI flake:
// a single degenerate turn used to fail the whole run terminally.
func TestEmptyResponseIsRetried(t *testing.T) {
	originalPrompt := rand.Text()
	var requests int
	retryRequest := make(chan []byte, 1)
	srv := newValidatingGenerateContentServer(t, nil, func(reqNum int, body []byte) string {
		requests = reqNum
		// One empty turn, then a real answer -- the shape of the transient.
		if reqNum == 1 {
			return emptyTurnJSON
		}
		if reqNum == 2 {
			retryRequest <- append([]byte(nil), body...)
		}
		return submitTurnJSON
	})

	exec := newEmptyResponseExecutor(t, srv.URL, originalPrompt, 10)

	resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Execute: got error %v, want the empty turn to be retried", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if requests < 2 {
		t.Errorf("requests: got = %d, want >= 2 (the retry must actually re-ask)", requests)
	}
	texts := requestTexts(t, <-retryRequest)
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, originalPrompt) {
		t.Errorf("retry request omitted original input: got text parts %q, want prompt %q", texts, originalPrompt)
	}
	if !strings.Contains(joined, "last response was empty") {
		t.Errorf("retry request omitted recovery instruction: got text parts %q", texts)
	}
}

// TestEmptyResponseRetryIsBounded proves the retry cannot mask a model that is
// genuinely stuck, and that the failure still names the real problem rather
// than degrading into the far less useful max-turns error.
func TestEmptyResponseRetryIsBounded(t *testing.T) {
	var requests int
	srv := newValidatingGenerateContentServer(t, nil, func(reqNum int, _ []byte) string {
		requests = reqNum
		return emptyTurnJSON
	})

	// maxTurns is deliberately far above the empty-response bound so that
	// hitting the bound, not the turn limit, is what stops the run.
	exec := newEmptyResponseExecutor(t, srv.URL, rand.Text(), 100)

	_, err := exec.Execute(t.Context(), errCapRequest{}, map[string]googletool.Metadata[errCapResponse]{})
	if err == nil {
		t.Fatal("Execute: got nil error, want failure after repeated empty responses")
	}
	if !strings.Contains(err.Error(), "unexpected response format from model") {
		t.Errorf("error: got = %q, want it to name the empty-response cause", err)
	}
	if strings.Contains(err.Error(), "maximum conversation turns") {
		t.Errorf("error: got = %q, want the specific empty-response error, not max-turns", err)
	}
	// 1 initial + 3 nudges. A runaway loop would blow far past this.
	if requests > 8 {
		t.Errorf("requests: got = %d, want the retry bounded well below maxTurns", requests)
	}
}
