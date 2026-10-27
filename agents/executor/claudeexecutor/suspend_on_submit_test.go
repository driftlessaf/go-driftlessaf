/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// newSuspendOnSubmitExecutor builds an executor WithSuspendOnSubmit and the
// ask_a_friend suspend tool against url.
func newSuspendOnSubmitExecutor(t *testing.T, url string) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	return newAskAndSubmitExecutor(t, url, claudeexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse]())
}

// newAskAndSubmitExecutor builds an executor with a submit tool, the ask_a_friend
// suspend tool and extra against url.
func newAskAndSubmitExecutor(t *testing.T, url string, extra ...claudeexecutor.Option[errCapRequest, errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	client := anthropic.NewClient(option.WithBaseURL(url), option.WithAPIKey("test"), option.WithMaxRetries(0))
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](9),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
	}, extra...)
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

// submitBlockSSE is a submit_result call answering answer.
func submitBlockSSE(t *testing.T, id, answer string) []string {
	t.Helper()
	return toolUseBlockSSE(t, 0, id, "submit_result", submitInput(answer))
}

// toolResultText returns the text of the tool_result for id in the request's
// last message, and whether there is one.
func toolResultText(t *testing.T, body []byte, id string) (string, bool) {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	last := req.Messages[len(req.Messages)-1]
	for _, c := range last.Content {
		if c.Type == "tool_result" && c.ToolUseID == id && len(c.Content) > 0 {
			return c.Content[0].Text, true
		}
	}
	return "", false
}

// TestSuspendOnSubmitResumesTheSameConversation: an accepted submission
// suspends with the submit call pending, and Resume continues the same
// conversation with the answer as the submit's result, through a second
// submission that suspends again.
func TestSuspendOnSubmitResumesTheSameConversation(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if n == 1 {
			return wrapTurn("msg_1", submitBlockSSE(t, "toolu_submit_1", "first")...)
		}
		return wrapTurn("msg_2", submitBlockSSE(t, "toolu_submit_2", "second")...)
	})
	exec := newSuspendOnSubmitExecutor(t, srv.URL)
	tools := map[string]claudetool.Metadata[errCapResponse]{}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a suspension", err)
	}
	if resp.Answer != "first" {
		t.Errorf("Execute's Response: got %q, want the accepted submission %q", resp.Answer, "first")
	}
	if susp.Reason != checkpoint.ReasonSubmitted {
		t.Errorf("Reason: got %q, want %q", susp.Reason, checkpoint.ReasonSubmitted)
	}
	if len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "toolu_submit_1" || susp.PendingToolCalls[0].Name != "submit_result" {
		t.Fatalf("PendingToolCalls: got %+v, want the one submit call", susp.PendingToolCalls)
	}
	if !strings.Contains(string(susp.PendingToolCalls[0].InputJSON), `"first"`) {
		t.Errorf("pending InputJSON: got %s, want the submission", susp.PendingToolCalls[0].InputJSON)
	}

	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor is not a Resumer")
	}
	resp, err = resumer.Resume(t.Context(), susp.Envelope, map[string]string{"toolu_submit_1": "sent back: name the gap"}, tools)
	again, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Resume: got %v, want the second submission's suspension", err)
	}
	if resp.Answer != "second" {
		t.Errorf("Resume's Response: got %q, want the accepted submission %q", resp.Answer, "second")
	}
	if got := again.PendingToolCalls[0].ID; got != "toolu_submit_2" {
		t.Errorf("second pending call: got %q, want toolu_submit_2", got)
	}
	if again.Turn != 1 {
		t.Errorf("second suspension's Turn: got %d, want 1 (the same conversation)", again.Turn)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests: got %d, want 2", len(bodies))
	}
	got, ok := toolResultText(t, bodies[1], "toolu_submit_1")
	if !ok || !strings.Contains(got, "sent back: name the gap") {
		t.Errorf("the resumed request's submit result: got %q (found %v), want the answer", got, ok)
	}
	if strings.Contains(got, "submitted successfully") {
		t.Errorf("the resumed request's submit result carries the commit message: %q", got)
	}
}

// TestSuspendOnSubmitAnswersASiblingAsk: an ask in the submitting turn is
// answered as not asked, so the resumed request pairs every call.
func TestSuspendOnSubmitAnswersASiblingAsk(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if n == 1 {
			blocks := submitBlockSSE(t, "toolu_submit", "done")
			blocks = append(blocks, toolUseBlockSSE(t, 1, "toolu_ask", askAFriendToolName, map[string]any{"reasoning": "r", "question": "ship it?"})...)
			return wrapTurn("msg_1", blocks...)
		}
		return wrapTurn("msg_2", submitBlockSSE(t, "toolu_submit_2", "again")...)
	})
	exec := newSuspendOnSubmitExecutor(t, srv.URL)
	tools := map[string]claudetool.Metadata[errCapResponse]{}

	_, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted {
		t.Fatalf("Execute: got %v, want the submission's suspension", err)
	}
	if len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "toolu_submit" {
		t.Fatalf("PendingToolCalls: got %+v, want only the submit call", susp.PendingToolCalls)
	}
	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor is not a Resumer")
	}
	if _, err := resumer.Resume(t.Context(), susp.Envelope, map[string]string{"toolu_submit": "accepted"}, tools); err == nil {
		t.Fatal("Resume: got nil, want the next suspension")
	}

	mu.Lock()
	defer mu.Unlock()
	got, ok := toolResultText(t, bodies[1], "toolu_ask")
	if !ok || !strings.Contains(got, "not_asked") {
		t.Errorf("the overtaken ask's result: got %q (found %v), want not_asked", got, ok)
	}
}

// TestSuspendOnSubmitRequiresASubmitTool: the option without a submit tool
// fails construction.
func TestSuspendOnSubmitRequiresASubmitTool(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	_, err = claudeexecutor.New[errCapRequest, errCapResponse](
		anthropic.NewClient(option.WithAPIKey("test")), prompt,
		claudeexecutor.WithSuspendOnSubmit[errCapRequest, errCapResponse](),
	)
	if err == nil || !strings.Contains(err.Error(), "requires a submit tool") {
		t.Errorf("New: got %v, want the missing-submit-tool error", err)
	}
}

// TestResumeWithResultsPlacesResultsVerbatim: ResumeWithResults pairs the
// caller's result with the pending call as it is, without the human-answer
// frame, and refuses a pending call it has no result for.
func TestResumeWithResultsPlacesResultsVerbatim(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		return wrapTurn("msg", submitBlockSSE(t, fmt.Sprintf("toolu_submit_%d", n), "x")...)
	})
	exec := newSuspendOnSubmitExecutor(t, srv.URL)
	tools := map[string]claudetool.Metadata[errCapResponse]{}
	_, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a suspension", err)
	}
	resumer, ok := exec.(claudeexecutor.ResultResumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor is not a ResultResumer")
	}
	for name, results := range map[string]map[string]string{
		"missing":         {},
		"whitespace-only": {susp.PendingToolCalls[0].ID: " \n\t"},
	} {
		if _, err := resumer.ResumeWithResults(t.Context(), susp.Envelope, results, tools); err == nil || !strings.Contains(err.Error(), "no result for pending call") {
			t.Fatalf("ResumeWithResults with a %s result: got %v, want a refusal", name, err)
		}
	}
	const review = "the reviewer's verdict: name the gap"
	if _, err := resumer.ResumeWithResults(t.Context(), susp.Envelope, map[string]string{susp.PendingToolCalls[0].ID: review}, tools); err == nil {
		t.Fatal("ResumeWithResults: got nil, want the next suspension")
	}
	mu.Lock()
	defer mu.Unlock()
	got, ok := toolResultText(t, bodies[1], susp.PendingToolCalls[0].ID)
	if !ok || got != review {
		t.Errorf("the resumed submit result: got %q (found %v), want %q verbatim", got, ok, review)
	}
}

// TestSuspendOnSubmitRefusesASecondSubmitInOneTurn: only the turn's first
// submission suspends and is reviewed; a second one is answered as an error.
func TestSuspendOnSubmitRefusesASecondSubmitInOneTurn(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := newValidatingAnthropicServer(t, func(n int, body []byte) []string {
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if n == 1 {
			return wrapTurn("msg_1", append(submitBlockSSE(t, "toolu_first", "first"), toolUseBlockSSE(t, 1, "toolu_second", "submit_result", submitInput("second"))...)...)
		}
		return wrapTurn("msg_2", submitBlockSSE(t, "toolu_third", "third")...)
	})
	exec := newSuspendOnSubmitExecutor(t, srv.URL)
	tools := map[string]claudetool.Metadata[errCapResponse]{}
	_, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "toolu_first" {
		t.Fatalf("Execute: got %v, want the first submission pending", err)
	}
	resumer, ok := exec.(claudeexecutor.ResultResumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor is not a ResultResumer")
	}
	if _, err := resumer.ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"toolu_first": "reviewed"}, tools); err == nil {
		t.Fatal("ResumeWithResults: got nil, want the next suspension")
	}
	mu.Lock()
	defer mu.Unlock()
	if got, ok := toolResultText(t, bodies[1], "toolu_second"); !ok || !strings.Contains(got, "only one submission per turn") {
		t.Errorf("the second submission's result: got %q (found %v), want the one-per-turn error", got, ok)
	}
}

// TestSuspendOnSubmitSeedSuspends: a seed submission suspends like one in a
// turn, before any model call.
func TestSuspendOnSubmitSeedSuspends(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		t.Error("the seeded submission reached the model")
		return nil
	})
	exec := newSuspendOnSubmitExecutor(t, srv.URL)
	input, err := json.Marshal(submitInput("seeded"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{},
		anthropic.ToolUseBlock{ID: "toolu_seed", Name: "submit_result", Input: input})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok || susp.Reason != checkpoint.ReasonSubmitted || susp.PendingToolCalls[0].ID != "toolu_seed" {
		t.Fatalf("Execute: got %v, want the seeded submission's suspension", err)
	}
	if resp.Answer != "seeded" {
		t.Errorf("Execute's Response: got %q, want the seeded submission %q", resp.Answer, "seeded")
	}
}

// TestSuspendOnSubmitIsInTheResumeDigest: an envelope from an executor with
// WithSuspendOnSubmit does not resume on one without it.
func TestSuspendOnSubmitIsInTheResumeDigest(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		return wrapTurn("msg", submitBlockSSE(t, "toolu_submit", "x")...)
	})
	tools := map[string]claudetool.Metadata[errCapResponse]{}
	_, err := newSuspendOnSubmitExecutor(t, srv.URL).Execute(t.Context(), errCapRequest{}, tools)
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a suspension", err)
	}
	plain := newAskAndSubmitExecutor(t, srv.URL)
	_, err = plain.(claudeexecutor.Resumer[errCapRequest, errCapResponse]).Resume(t.Context(), susp.Envelope, map[string]string{"toolu_submit": "x"}, tools)
	if !errors.Is(err, checkpoint.ErrConfigDrift) {
		t.Errorf("Resume without the option: got %v, want ErrConfigDrift", err)
	}
}

// TestSuspendOnSubmitReturnsTheRepairedSubmission: the Response returned with
// the suspension is the submission as the submit tool accepted it, here a
// payload the model sent as a JSON string, while the pending call keeps the
// raw input.
func TestSuspendOnSubmitReturnsTheRepairedSubmission(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		return wrapTurn("msg", toolUseBlockSSE(t, 0, "toolu_submit", "submit_result", map[string]any{
			"reasoning": "done",
			"result":    `{"answer":"stringified"}`,
		})...)
	})
	resp, err := newSuspendOnSubmitExecutor(t, srv.URL).Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a suspension", err)
	}
	if resp.Answer != "stringified" {
		t.Errorf("Execute's Response: got %q, want the repaired submission %q", resp.Answer, "stringified")
	}
	var raw struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(susp.PendingToolCalls[0].InputJSON, &raw); err != nil {
		t.Fatalf("unmarshal pending InputJSON: %v", err)
	}
	if _, isString := raw.Result.(string); !isString {
		t.Errorf("pending InputJSON result: got %T, want the raw string the model sent", raw.Result)
	}
}
