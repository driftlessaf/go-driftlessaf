/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// refusalTurn is the SSE event sequence for a turn Anthropic's safety
// classifier refused: message_start carries stop_reason null as usual, but
// message_delta reports stop_reason "refusal" with stop_details naming the
// policy category, and content is empty throughout.
var refusalTurn = []string{
	`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
	`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"matched a cyber policy pattern"}},"usage":{"output_tokens":0}}`,
	`{"type":"message_stop"}`,
}

// answerTurn is a normal final answer the fallback JSON parser accepts.
var answerTurn = []string{
	`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":20,"output_tokens":5}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"42\"}"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
	`{"type":"message_stop"}`,
}

func newExecTestClient(t *testing.T, turnsByRequest func(n int) []string) (anthropic.Client, func() [][]byte) {
	t.Helper()
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

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, turnsByRequest(n)))
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	return client, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

// TestRefusalWithoutNudgeFailsImmediately pins the default (WithRefusalNudge
// not set): a refused turn fails the run on the first refusal, and the error
// is a *claudeexecutor.RefusalError carrying Anthropic's category — not the
// generic "no content in Claude's response" string a refusal used to produce
// indistinguishably from any other empty completion.
func TestRefusalWithoutNudgeFailsImmediately(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return refusalTurn })

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)
	if execErr == nil {
		t.Fatal("Execute: got nil error, want a RefusalError")
	}
	var refErr *claudeexecutor.RefusalError
	if !errors.As(execErr, &refErr) {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.RefusalError", execErr, execErr)
	}
	if got, want := refErr.Category, "cyber"; got != want {
		t.Errorf("RefusalError.Category: got = %q, want = %q", got, want)
	}
	if refErr.Explanation == "" {
		t.Error("RefusalError.Explanation: got empty, want Anthropic's explanation text preserved")
	}

	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1 (no retry without WithRefusalNudge)", got)
	}
}

// TestRefusalNudgeRetriesThenSucceeds pins the WithRefusalNudge recovery
// path: a refused first turn is retried with a nudge instead of failing the
// run, and a normal second turn then completes it. It also pins the request
// shape sent on retry: a placeholder assistant turn (never message.ToParam()
// on an empty-content message, which is itself an invalid request) followed
// by a user-role nudge naming the category — preserving the API's required
// role alternation.
func TestRefusalNudgeRetriesThenSucceeds(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return refusalTurn
		}
		return answerTurn
	})

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithRefusalNudge[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}

	reqs := requests()
	if got := len(reqs); got != 2 {
		t.Fatalf("HTTP requests: got = %d, want = 2 (one refusal, one retry)", got)
	}

	var second struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[1], &second); err != nil {
		t.Fatalf("unmarshal second request: %v", err)
	}

	if len(second.Messages) < 3 {
		t.Fatalf("second request messages: got = %d, want >= 3 (initial user, placeholder assistant, nudge user)", len(second.Messages))
	}
	// Roles must strictly alternate; verify the two turns the retry appended.
	placeholder := second.Messages[len(second.Messages)-2]
	nudge := second.Messages[len(second.Messages)-1]
	if placeholder.Role != "assistant" {
		t.Errorf("second-to-last message role: got = %q, want = %q", placeholder.Role, "assistant")
	}
	if nudge.Role != "user" {
		t.Errorf("last message role: got = %q, want = %q", nudge.Role, "user")
	}
	var nudgeText string
	for _, cb := range nudge.Content {
		if cb.Type == "text" {
			nudgeText = cb.Text
		}
	}
	if !strings.Contains(nudgeText, "cyber") {
		t.Errorf("nudge text: got = %q, want it to name the refusal category %q", nudgeText, "cyber")
	}
	if strings.Contains(nudgeText, "matched a cyber policy pattern") {
		t.Error("nudge text echoes Anthropic's explanation verbatim; it must not surface classifier-triggering detail back into the conversation")
	}
}

// TestRefusalOnFinalTurnReturnsTypedError pins the interaction between the
// nudge and the turn budget: a refusal on the conversation's final turn must
// fail as a *RefusalError, not schedule a retry the loop has no turn left to
// send — which would surface as the generic "exceeded maximum conversation
// turns" error and lose the typed-error guarantee.
func TestRefusalOnFinalTurnReturnsTypedError(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return refusalTurn })

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](1),
		claudeexecutor.WithRefusalNudge[errCapRequest, errCapResponse](1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)
	if execErr == nil {
		t.Fatal("Execute: got nil error, want a RefusalError")
	}
	var refErr *claudeexecutor.RefusalError
	if !errors.As(execErr, &refErr) {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.RefusalError", execErr, execErr)
	}
	if got, want := refErr.Category, "cyber"; got != want {
		t.Errorf("RefusalError.Category: got = %q, want = %q", got, want)
	}

	// No retry can be scheduled with zero turns remaining, so exactly one
	// request reaches the API.
	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1 (no retry on the final turn)", got)
	}
}
