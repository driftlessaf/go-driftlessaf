/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// submitCallTurn renders the SSE events for an assistant turn that calls
// submit_result with the given input object.
func submitCallTurn(t *testing.T, msgID, callID string, input map[string]any) []string {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	partial, err := json.Marshal(string(inputJSON))
	if err != nil {
		t.Fatalf("marshal partial: %v", err)
	}
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":"submit_result","input":{}}}`, callID),
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
}

// submitInput builds a {reasoning, result:{answer}} submit_result input.
func submitInput(answer string) map[string]any {
	return map[string]any{
		"reasoning": "the analysis is complete",
		"result":    map[string]any{"answer": answer},
	}
}

// newSubmitExecutor builds an executor against srv with the real submitresult
// tool for errCapResponse and the given validators.
func newSubmitExecutor(t *testing.T, srv *httptest.Server, validators ...callbacks.ResultValidator[errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	opts := []claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
	}
	for _, v := range validators {
		opts = append(opts, claudeexecutor.WithResultValidator[errCapRequest, errCapResponse](v))
	}

	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
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

		events := submitCallTurn(t, "msg_01", "toolu_s1", submitInput("wrong"))
		if n > 1 {
			events = submitCallTurn(t, "msg_02", "toolu_s2", submitInput("correct"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)

	rejectWrong := func(_ context.Context, r errCapResponse, _ string) ([]callbacks.Finding, error) {
		if r.Answer != "correct" {
			return []callbacks.Finding{{
				Kind:       callbacks.FindingKindReview,
				Identifier: "wrong-answer",
				Details:    "the answer is not correct",
			}}, nil
		}
		return nil, nil
	}

	exec := newSubmitExecutor(t, srv, rejectWrong)

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

	// The second request must carry the rejection back to the model: the
	// findings and the identifier the validator raised.
	second := string(requests[1])
	for _, want := range []string{"findings", "wrong-answer", "Result rejected"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request missing %q in rejection tool result:\n%s", want, second)
		}
	}
}

// TestSubmitZeroValueResponseCommits pins the explicit-commit semantics: a
// submission whose payload parses to the zero Response still ends the run —
// the model was told "submitted successfully", so the run must not keep going.
func TestSubmitZeroValueResponseCommits(t *testing.T) {
	var mu sync.Mutex
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, "msg_01", "toolu_s1", submitInput(""))))
	}))
	t.Cleanup(srv.Close)

	exec := newSubmitExecutor(t, srv)

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, ""; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := requestCount, 1; got != want {
		t.Errorf("API requests: got = %d, want = %d (zero-value submission must terminate the run)", got, want)
	}
}

// TestSubmitValidatorErrorAbortsRun pins the fail-loud contract: a validator
// that itself errors (as opposed to rejecting the result) aborts the run.
func TestSubmitValidatorErrorAbortsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, "msg_01", "toolu_s1", submitInput("fine"))))
	}))
	t.Cleanup(srv.Close)

	exec := newSubmitExecutor(t, srv, func(context.Context, errCapResponse, string) ([]callbacks.Finding, error) {
		return nil, errors.New("judge unavailable")
	})

	_, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err == nil {
		t.Fatal("Execute: got = nil, want = validator error")
	}
	if !strings.Contains(err.Error(), "result validation") {
		t.Errorf("Execute error: got = %v, want mention of result validation", err)
	}
}

// TestSubmitAcceptedWithValidatorsCommits confirms the happy path with
// validators registered: a submission every validator accepts commits as the
// run's final result on the first turn.
func TestSubmitAcceptedWithValidatorsCommits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, "msg_01", "toolu_s1", submitInput("fine"))))
	}))
	t.Cleanup(srv.Close)

	var gotReasoning string
	exec := newSubmitExecutor(t, srv, func(_ context.Context, _ errCapResponse, reasoning string) ([]callbacks.Finding, error) {
		gotReasoning = reasoning
		return nil, nil
	})

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "fine"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	if got, want := gotReasoning, "the analysis is complete"; got != want {
		t.Errorf("validator reasoning: got = %q, want = %q", got, want)
	}
}

// TestCallerToolShadowsSubmitTool preserves the precedence contract: a
// caller-registered tool with the submit tool's name wins — its handler runs
// and the configured submit tool is not advertised twice.
func TestCallerToolShadowsSubmitTool(t *testing.T) {
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
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, "msg_01", "toolu_s1", submitInput("shadowed"))))
	}))
	t.Cleanup(srv.Close)

	exec := newSubmitExecutor(t, srv)

	tools := map[string]claudetool.Metadata[errCapResponse]{
		"submit_result": {
			Definition: anthropic.ToolParam{Name: "submit_result"},
			Handler: func(_ context.Context, _ anthropic.ToolUseBlock, _ *agenttrace.Trace[errCapResponse], result *errCapResponse) map[string]any {
				*result = errCapResponse{Answer: "from-shadow"}
				return map[string]any{"ok": true}
			},
		},
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "from-shadow"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}

	// The advertised tool set must contain submit_result exactly once.
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Count(string(requests[0]), `"name":"submit_result"`), 1; got != want {
		t.Errorf("submit_result advertised %d times, want %d", got, want)
	}
}

// gradedResponse carries a jsonschema enum so the base schema-conformance
// validator — installed by default, with no WithResultValidator registered —
// has a declared constraint to enforce.
type gradedResponse struct {
	Answer string `json:"answer" jsonschema:"required,enum=yes,enum=no"`
}

// TestBaseSchemaValidatorRejectsNonconformingSubmission pins the built-in
// schema gate: a submission that parses but violates the response type's
// declared schema is rejected back to the model with the violation named,
// and the loop continues until a conforming submission commits.
func TestBaseSchemaValidatorRejectsNonconformingSubmission(t *testing.T) {
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

		answer := "maybe"
		if n > 1 {
			answer = "yes"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, submitCallTurn(t, fmt.Sprintf("msg_%02d", n), fmt.Sprintf("toolu_s%d", n), map[string]any{
			"reasoning": "the analysis is complete",
			"result":    map[string]any{"answer": answer},
		})))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, gradedResponse](client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, gradedResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, gradedResponse](5),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, gradedResponse](submitresult.ClaudeToolForResponse[gradedResponse]),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "yes"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := len(requests), 2; got != want {
		t.Fatalf("API requests: got = %d, want = %d", got, want)
	}

	// The rejection returned to the model names the violated constraint.
	second := string(requests[1])
	for _, want := range []string{"schema:answer", "allowed values", "Result rejected"} {
		if !strings.Contains(second, want) {
			t.Errorf("second request missing %q in rejection tool result:\n%s", want, second)
		}
	}
}
