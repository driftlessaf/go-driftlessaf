/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// maxTokensTurn is the SSE event sequence for a turn that spent its whole
// output budget on thinking: a redacted_thinking block is the only content,
// and message_delta reports stop_reason "max_tokens" with output_tokens equal
// to the configured cap.
var maxTokensTurn = []string{
	`{"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":2,"output_tokens":0}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":32000}}`,
	`{"type":"message_stop"}`,
}

// endTurnEmpty is an end_turn stop that carries no content at all.
var endTurnEmpty = []string{
	`{"type":"message_start","message":{"id":"msg_04","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":2,"output_tokens":0}}}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
	`{"type":"message_stop"}`,
}

// execNoContent runs a single-turn execution against a fake server that
// replies with the given events and returns the Execute error.
func execNoContent(t *testing.T, turn []string) error {
	t.Helper()
	client, requests := newExecTestClient(t, func(int) []string { return turn })

	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, execErr := exec.Execute(t.Context(), errCapRequest{}, nil)
	if execErr == nil {
		t.Fatal("Execute: got nil error, want a no-content error")
	}
	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1 (no-content stops are not retried by the executor)", got)
	}
	return execErr
}

// TestMaxTokensNoContentIsTyped pins that a max_tokens stop with no content
// surfaces as *MaxTokensError carrying the configured cap and the emitted
// output tokens, while its text keeps the legacy "no content" prefix that
// substring-matching callers depend on.
func TestMaxTokensNoContentIsTyped(t *testing.T) {
	execErr := execNoContent(t, maxTokensTurn)

	mtErr, ok := errors.AsType[*claudeexecutor.MaxTokensError](execErr)
	if !ok {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.MaxTokensError", execErr, execErr)
	}
	if got, want := mtErr.OutputTokens, int64(32000); got != want {
		t.Errorf("MaxTokensError.OutputTokens: got = %d, want = %d", got, want)
	}
	if got, want := mtErr.MaxTokens, int64(32000); got != want {
		t.Errorf("MaxTokensError.MaxTokens: got = %d, want = %d", got, want)
	}
	if !strings.Contains(execErr.Error(), "no content in Claude's response") {
		t.Errorf("Execute error text: got %q, want it to contain the legacy no-content text", execErr.Error())
	}
	if _, isRefusal := errors.AsType[*claudeexecutor.RefusalError](execErr); isRefusal {
		t.Errorf("Execute error: got a RefusalError for a max_tokens stop: %v", execErr)
	}
}

// TestRefusalStaysAheadOfMaxTokens pins the branch ordering: a refusal is
// classified as *RefusalError and never reaches the max_tokens branch.
func TestRefusalStaysAheadOfMaxTokens(t *testing.T) {
	execErr := execNoContent(t, refusalTurn)

	if _, ok := errors.AsType[*claudeexecutor.RefusalError](execErr); !ok {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.RefusalError", execErr, execErr)
	}
	if _, isMaxTokens := errors.AsType[*claudeexecutor.MaxTokensError](execErr); isMaxTokens {
		t.Errorf("Execute error: got a MaxTokensError for a refusal stop: %v", execErr)
	}
}

// TestEndTurnNoContentStaysUntyped pins that other stop reasons with no
// content keep the untyped fallback error.
func TestEndTurnNoContentStaysUntyped(t *testing.T) {
	execErr := execNoContent(t, endTurnEmpty)

	if _, isMaxTokens := errors.AsType[*claudeexecutor.MaxTokensError](execErr); isMaxTokens {
		t.Errorf("Execute error: got a MaxTokensError for an end_turn stop: %v", execErr)
	}
	if _, isRefusal := errors.AsType[*claudeexecutor.RefusalError](execErr); isRefusal {
		t.Errorf("Execute error: got a RefusalError for an end_turn stop: %v", execErr)
	}
	if got, want := execErr.Error(), "no content in Claude's response"; !strings.Contains(got, want) {
		t.Errorf("Execute error text: got %q, want it to contain %q", got, want)
	}
}
