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
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
)

// countLinesTool is the caller tool the fixtures below call.
const countLinesTool = "count_lines"

// toolCallTurn is the SSE event sequence for a turn that calls tool with the
// given partial_json and stops with stopReason. A cut-off call carries partial
// JSON and stop_reason max_tokens; a complete call carries a whole object and
// stop_reason tool_use. An empty partialJSON emits no input_json_delta at all,
// the shape of a call cut before its first input byte.
func toolCallTurn(msgID, callID, tool, partialJSON, stopReason string, outputTokens int) []string {
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, callID, tool),
	}
	if partialJSON != "" {
		partial, _ := json.Marshal(partialJSON)
		events = append(events, fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial))
	}
	return append(events,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stopReason, outputTokens),
		`{"type":"message_stop"}`,
	)
}

// truncatedToolTurn is a count_lines call the output cap cut off mid input.
func truncatedToolTurn(msgID, callID string) []string {
	return toolCallTurn(msgID, callID, countLinesTool, `{"reasoning":"count them","path":"/var/lo`, "max_tokens", 32000)
}

// completeToolTurn is an ordinary, whole count_lines call.
func completeToolTurn(msgID, callID string) []string {
	return toolCallTurn(msgID, callID, countLinesTool, `{"reasoning":"count them","path":"/var/log"}`, "tool_use", 40)
}

// siblingsTurn is a turn with two calls: a complete count_lines call followed
// by a second one the cap cut off.
func siblingsTurn(msgID, completeID, cutID string) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, completeID, countLinesTool),
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"count them\",\"path\":\"/var/log\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, cutID, countLinesTool),
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"and the oth"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":32000}}`,
		`{"type":"message_stop"}`,
	}
}

// countingTools registers countLinesTool with a handler that counts its
// calls, so a test can assert a cut-off call never reached it.
func countingTools(calls *atomic.Int32) map[string]claudetool.Metadata[errCapResponse] {
	return map[string]claudetool.Metadata[errCapResponse]{
		countLinesTool: {
			Definition: anthropic.ToolParam{Name: countLinesTool},
			Handler: func(context.Context, anthropic.ToolUseBlock, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				calls.Add(1)
				return map[string]any{"lines": 3}
			},
		},
	}
}

// newTruncationExecutor builds an executor with the fast retry config, a 32000
// output cap, the given turn budget, and any extra options.
func newTruncationExecutor(t *testing.T, client anthropic.Client, maxTurns int, extra ...claudeexecutor.Option[errCapRequest, errCapResponse]) claudeexecutor.Interface[errCapRequest, errCapResponse] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	opts := append([]claudeexecutor.Option[errCapRequest, errCapResponse]{
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
		claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
	}, extra...)
	exec, err := claudeexecutor.New(client, prompt, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exec
}

// requestMessage is one message of a decoded Messages API request body, with
// the content fields the tests below assert on.
type requestMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
}

// requestMessages decodes the messages of a captured request body.
func requestMessages(t *testing.T, body []byte) []requestMessage {
	t.Helper()
	var req struct {
		Messages []requestMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return req.Messages
}

// wantTruncatedError asserts err is a *TruncatedToolCallError naming
// countLinesTool with the cap and the emitted tokens the fixtures report, and
// that it matches *MaxTokensError through Unwrap.
func wantTruncatedError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Execute: got nil error, want a TruncatedToolCallError")
	}
	terr, ok := errors.AsType[*claudeexecutor.TruncatedToolCallError](err)
	if !ok {
		t.Fatalf("Execute error: got %v (%T), want *claudeexecutor.TruncatedToolCallError", err, err)
	}
	if got, want := terr.Tool, countLinesTool; got != want {
		t.Errorf("TruncatedToolCallError.Tool: got = %q, want = %q", got, want)
	}
	merr, ok := errors.AsType[*claudeexecutor.MaxTokensError](err)
	if !ok {
		t.Fatalf("Execute error: got %v, want it to match *claudeexecutor.MaxTokensError through Unwrap", err)
	}
	if got, want := merr.OutputTokens, int64(32000); got != want {
		t.Errorf("MaxTokensError.OutputTokens: got = %d, want = %d", got, want)
	}
	if got, want := merr.MaxTokens, int64(32000); got != want {
		t.Errorf("MaxTokensError.MaxTokens: got = %d, want = %d", got, want)
	}
	if !strings.Contains(err.Error(), countLinesTool) {
		t.Errorf("Execute error text: got %q, want it to name the tool %q", err.Error(), countLinesTool)
	}
	if strings.Contains(err.Error(), "no content in Claude's response") {
		t.Errorf("Execute error text: got %q, want no no-content prefix on a turn that carried a tool call", err.Error())
	}
}

// recordedCalls returns the recorded tool calls named tool from the single
// trace the tracer captured.
func recordedCalls(t *testing.T, tracer *recordingTracer, tool string) []*agenttrace.ToolCall[errCapResponse] {
	t.Helper()
	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	var calls []*agenttrace.ToolCall[errCapResponse]
	for _, tc := range tracer.traces[0].ToolCalls {
		if tc.Name == tool {
			calls = append(calls, tc)
		}
	}
	return calls
}

// TestTruncatedToolCallAnsweredThenRecovers pins the default recovery path:
// the cut-off call is never dispatched, the model's own message is replayed
// with the call's input emptied, the call is answered with an error
// tool_result naming the tool and the limit, and a normal next turn completes
// the run. A work tool's cut-off call is recorded as a plain bad call.
func TestTruncatedToolCallAnsweredThenRecovers(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return truncatedToolTurn("msg_t1", "toolu_t1")
		}
		return answerTurn
	})
	var calls atomic.Int32
	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	resp, err := newTruncationExecutor(t, client, 3).Execute(ctx, errCapRequest{}, countingTools(&calls))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0 (a cut-off call is never dispatched)", got)
	}

	reqs := requests()
	if got := len(reqs); got != 2 {
		t.Fatalf("HTTP requests: got = %d, want = 2 (one cut-off turn, one retry)", got)
	}
	msgs := requestMessages(t, reqs[1])
	if len(msgs) < 3 {
		t.Fatalf("retry request messages: got = %d, want >= 3 (initial user, replayed assistant, tool results)", len(msgs))
	}
	assistant, results := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" || assistant.Content[0].ID != "toolu_t1" {
		t.Errorf("replayed assistant turn: got = %+v, want the model's own tool_use toolu_t1", assistant)
	}
	if got := string(assistant.Content[0].Input); got != "{}" {
		t.Errorf("replayed tool_use input: got = %s, want = {} (the partial JSON must not reach the API)", got)
	}
	if results.Role != "user" || len(results.Content) != 1 {
		t.Fatalf("results turn: got = %+v, want one user-role tool_result", results)
	}
	res := results.Content[0]
	if res.Type != "tool_result" || res.ToolUseID != "toolu_t1" || !res.IsError {
		t.Errorf("tool_result: got type=%q tool_use_id=%q is_error=%v, want an error tool_result for toolu_t1", res.Type, res.ToolUseID, res.IsError)
	}
	var text string
	if len(res.Content) > 0 {
		text = res.Content[0].Text
	}
	for _, want := range []string{countLinesTool, "32000"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool_result text: got = %q, want it to contain %q", text, want)
		}
	}

	recorded := recordedCalls(t, tracer, countLinesTool)
	if len(recorded) != 1 {
		t.Fatalf("recorded %s calls: got = %d, want = 1 (the cut-off call)", countLinesTool, len(recorded))
	}
	if recorded[0].Recoverable {
		t.Error("recorded call Recoverable: got = true, want = false (a work tool's cut-off call is not the terminal submit)")
	}
	if _, ok := errors.AsType[*claudeexecutor.TruncatedToolCallError](recorded[0].Error); !ok {
		t.Errorf("recorded call error: got %v (%T), want *claudeexecutor.TruncatedToolCallError", recorded[0].Error, recorded[0].Error)
	}
}

// TestTruncatedToolCallKeepsSiblings pins that a cut-off call holds only
// itself out of dispatch: the complete call in the same turn runs, and the
// retry request carries its real result beside the error result.
func TestTruncatedToolCallKeepsSiblings(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return siblingsTurn("msg_s1", "toolu_ok", "toolu_cut")
		}
		return answerTurn
	})
	var calls atomic.Int32

	if _, err := newTruncationExecutor(t, client, 3).Execute(t.Context(), errCapRequest{}, countingTools(&calls)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("handler calls: got = %d, want = 1 (the complete sibling runs)", got)
	}
	reqs := requests()
	if got := len(reqs); got != 2 {
		t.Fatalf("HTTP requests: got = %d, want = 2", got)
	}
	msgs := requestMessages(t, reqs[1])
	results := msgs[len(msgs)-1]
	if len(results.Content) != 2 {
		t.Fatalf("results turn content: got = %d blocks, want = 2 (the sibling's result and the cut-off call's error result)", len(results.Content))
	}
	if got := results.Content[0]; got.ToolUseID != "toolu_ok" || got.IsError {
		t.Errorf("first result: got tool_use_id=%q is_error=%v, want the sibling's real result for toolu_ok", got.ToolUseID, got.IsError)
	}
	if got := results.Content[1]; got.ToolUseID != "toolu_cut" || !got.IsError {
		t.Errorf("second result: got tool_use_id=%q is_error=%v, want the error result for toolu_cut", got.ToolUseID, got.IsError)
	}
}

// TestTruncatedToolCallWithoutInputIsCut pins the shape of a call cut before
// its first input byte: no input_json_delta at all, stop_reason max_tokens.
func TestTruncatedToolCallWithoutInputIsCut(t *testing.T) {
	client, _ := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return toolCallTurn("msg_e1", "toolu_e1", countLinesTool, "", "max_tokens", 32000)
		}
		return answerTurn
	})
	var calls atomic.Int32

	if _, err := newTruncationExecutor(t, client, 3).Execute(t.Context(), errCapRequest{}, countingTools(&calls)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0", got)
	}
}

// TestTruncatedToolCallTwiceReturnsTypedError pins the bound: a second cut-off
// call in the run fails it as a *TruncatedToolCallError, with neither call
// dispatched and both recorded as bad calls.
func TestTruncatedToolCallTwiceReturnsTypedError(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		return truncatedToolTurn(fmt.Sprintf("msg_t%d", n), fmt.Sprintf("toolu_t%d", n))
	})
	var calls atomic.Int32
	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	_, err := newTruncationExecutor(t, client, 3).Execute(ctx, errCapRequest{}, countingTools(&calls))
	wantTruncatedError(t, err)
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0", got)
	}
	if got := len(requests()); got != 2 {
		t.Errorf("HTTP requests: got = %d, want = 2 (the cut-off turn and its one retry)", got)
	}
	recorded := recordedCalls(t, tracer, countLinesTool)
	if len(recorded) != 2 {
		t.Fatalf("recorded calls: got = %d, want = 2", len(recorded))
	}
	for i, tc := range recorded {
		if tc.Recoverable {
			t.Errorf("recorded call %d Recoverable: got = true, want = false", i)
		}
	}
}

// TestTruncatedToolCallBoundIsPerRun pins that the bound counts the whole run:
// a dispatched call between two cut-off calls does not restore the retry.
func TestTruncatedToolCallBoundIsPerRun(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		switch n {
		case 1:
			return truncatedToolTurn("msg_t1", "toolu_t1")
		case 2:
			return completeToolTurn("msg_c2", "toolu_c2")
		default:
			return truncatedToolTurn("msg_t3", "toolu_t3")
		}
	})
	var calls atomic.Int32

	_, err := newTruncationExecutor(t, client, 6).Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	wantTruncatedError(t, err)
	if got := calls.Load(); got != 1 {
		t.Errorf("handler calls: got = %d, want = 1 (only the complete call is dispatched)", got)
	}
	if got := len(requests()); got != 3 {
		t.Errorf("HTTP requests: got = %d, want = 3 (cut-off, complete, cut-off again)", got)
	}
}

// TestTruncatedToolCallRetriesDisabled pins WithTruncatedToolCallRetries(0):
// the first cut-off call fails the run with no retry.
func TestTruncatedToolCallRetriesDisabled(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return truncatedToolTurn("msg_t1", "toolu_t1") })
	var calls atomic.Int32

	_, err := newTruncationExecutor(t, client, 3,
		claudeexecutor.WithTruncatedToolCallRetries[errCapRequest, errCapResponse](0),
	).Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	wantTruncatedError(t, err)
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0", got)
	}
	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1 (no retry when disabled)", got)
	}
}

// TestTruncatedToolCallOnFinalTurnReturnsTypedError pins the interaction with
// the turn budget: a cut-off call on the final turn fails as a
// *TruncatedToolCallError, not as a retry the loop has no turn left to send.
func TestTruncatedToolCallOnFinalTurnReturnsTypedError(t *testing.T) {
	client, requests := newExecTestClient(t, func(int) []string { return truncatedToolTurn("msg_t1", "toolu_t1") })
	var calls atomic.Int32

	_, err := newTruncationExecutor(t, client, 1).Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	wantTruncatedError(t, err)
	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1 (no retry on the final turn)", got)
	}
}

// TestCompleteToolCallAtMaxTokensDispatches pins the boundary of the
// detection: a whole, non-empty call that closes exactly at the cap is an
// ordinary call and is dispatched.
func TestCompleteToolCallAtMaxTokensDispatches(t *testing.T) {
	client, requests := newExecTestClient(t, func(n int) []string {
		if n == 1 {
			return toolCallTurn("msg_c1", "toolu_c1", countLinesTool, `{"reasoning":"count them","path":"/var/log"}`, "max_tokens", 32000)
		}
		return answerTurn
	})
	var calls atomic.Int32

	resp, err := newTruncationExecutor(t, client, 3).Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("handler calls: got = %d, want = 1 (a complete call is dispatched)", got)
	}
	if got := len(requests()); got != 2 {
		t.Errorf("HTTP requests: got = %d, want = 2", got)
	}
}

// TestTruncatedSubmitCallIsRecoverable pins the one case the trace contract
// admits as recoverable: a cut-off terminal submit call, which a run that
// never recovers cannot complete without. The retry then submits normally.
func TestTruncatedSubmitCallIsRecoverable(t *testing.T) {
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

		events := toolCallTurn("msg_s1", "toolu_s1", "submit_result", `{"reasoning":"done","result":{"answ`, "max_tokens", 32000)
		if n > 1 {
			events = submitCallTurn(t, "msg_s2", "toolu_s2", submitInput("done"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(t, events))
	}))
	t.Cleanup(srv.Close)
	tracer := &recordingTracer{}
	ctx := agenttrace.WithTracer[errCapResponse](t.Context(), tracer)

	resp, err := newSubmitExecutor(t, srv).Execute(ctx, errCapRequest{}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}
	mu.Lock()
	n := len(requests)
	mu.Unlock()
	if n != 2 {
		t.Errorf("HTTP requests: got = %d, want = 2", n)
	}
	var recoverable int
	for _, tc := range recordedCalls(t, tracer, "submit_result") {
		if tc.Recoverable {
			recoverable++
		}
	}
	if recoverable != 1 {
		t.Errorf("recoverable submit_result records: got = %d, want = 1 (the cut-off submit)", recoverable)
	}
}

// TestTruncatedToolCallSiblingSubmitWins pins precedence on the turn that
// would end the run: a complete terminal submit beside the cut-off call still
// commits the result, so the run succeeds instead of failing with the typed
// error.
func TestTruncatedToolCallSiblingSubmitWins(t *testing.T) {
	submit, err := json.Marshal(submitInput("done"))
	if err != nil {
		t.Fatalf("marshal submit input: %v", err)
	}
	submitPartial, _ := json.Marshal(string(submit))
	turn := []string{
		`{"type":"message_start","message":{"id":"msg_w1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_sub","name":"submit_result","input":{}}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, submitPartial),
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_cut","name":%q,"input":{}}}`, countLinesTool),
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"reasoning\":\"and th"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":32000}}`,
		`{"type":"message_stop"}`,
	}
	client, requests := newExecTestClient(t, func(int) []string { return turn })
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New(client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithMaxTokens[errCapRequest, errCapResponse](32000),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithTruncatedToolCallRetries[errCapRequest, errCapResponse](0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var calls atomic.Int32

	resp, err := exec.Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	if err != nil {
		t.Fatalf("Execute: %v (a complete sibling submit must win over the cut-off call's terminal error)", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0", got)
	}
	if got := len(requests()); got != 1 {
		t.Errorf("HTTP requests: got = %d, want = 1", got)
	}
}

// TestTruncatedToolCallBoundSurvivesResume pins that the per-run bound is
// carried through a suspension: a cut-off call, then an ask-a-friend park,
// then a cut-off call on the resumed run fails with the typed error and no
// further model request, because the retry was already spent before the park.
// The same envelope with its loop state replaced by another writer's object,
// the shape a caller that stamps its own record into LoopState produces,
// resumes with a fresh count and gets the retry; a corrupt loop state fails
// the resume instead.
func TestTruncatedToolCallBoundSurvivesResume(t *testing.T) {
	askTurn := wrapTurn("msg_ask",
		toolUseBlockSSE(t, 0, "toolu_ask", askAFriendToolName,
			map[string]any{"reasoning": "need a decision", "question": "continue?"})...)
	client, requests := newExecTestClient(t, func(n int) []string {
		switch n {
		case 1:
			return truncatedToolTurn("msg_t1", "toolu_t1")
		case 2:
			return askTurn
		case 3:
			return truncatedToolTurn("msg_t3", "toolu_t3")
		case 4:
			return truncatedToolTurn("msg_t4", "toolu_t4")
		default:
			return answerTurn
		}
	})
	var calls atomic.Int32
	exec := newTruncationExecutor(t, client, 8,
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
	)

	_, err := exec.Execute(t.Context(), errCapRequest{}, countingTools(&calls))
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got %v, want a *checkpoint.Suspension", err)
	}
	if len(susp.LoopState) == 0 {
		t.Fatal("Suspension.LoopState: got empty, want the truncation count carried")
	}
	resumer, ok := exec.(claudeexecutor.Resumer[errCapRequest, errCapResponse])
	if !ok {
		t.Fatal("executor does not satisfy Resumer")
	}
	answers := map[string]string{"toolu_ask": "yes"}

	_, err = resumer.Resume(t.Context(), susp.Envelope, answers, countingTools(&calls))
	wantTruncatedError(t, err)
	if got := len(requests()); got != 3 {
		t.Errorf("HTTP requests: got = %d, want = 3 (cut-off, park, cut-off on resume with no retry)", got)
	}

	foreign := susp.Envelope
	foreign.LoopState = json.RawMessage(`{"round":3}`)
	resp, err := resumer.Resume(t.Context(), foreign, answers, countingTools(&calls))
	if err != nil {
		t.Fatalf("Resume with a foreign loop state: %v", err)
	}
	if got, want := resp.Answer, "42"; got != want {
		t.Errorf("answer: got = %q, want = %q", got, want)
	}
	if got := len(requests()); got != 5 {
		t.Errorf("HTTP requests: got = %d, want = 5 (a foreign loop state resumes with a fresh count and gets the retry)", got)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("handler calls: got = %d, want = 0", got)
	}

	corrupt := susp.Envelope
	corrupt.LoopState = json.RawMessage(`{not json`)
	if _, err := resumer.Resume(t.Context(), corrupt, answers, countingTools(&calls)); err == nil || !strings.Contains(err.Error(), "loop state") {
		t.Errorf("Resume with a corrupt loop state: got %v, want an error naming the loop state", err)
	}
	if got := len(requests()); got != 5 {
		t.Errorf("HTTP requests: got = %d, want = 5 (a corrupt loop state fails before any request)", got)
	}
}

// TestWithTruncatedToolCallRetriesRejectsNegative pins the option's guard.
func TestWithTruncatedToolCallRetriesRejectsNegative(t *testing.T) {
	client, _ := newExecTestClient(t, func(int) []string { return answerTurn })
	prompt, err := promptbuilder.NewPrompt("hello")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	if _, err := claudeexecutor.New[errCapRequest, errCapResponse](client, prompt,
		claudeexecutor.WithTruncatedToolCallRetries[errCapRequest, errCapResponse](-1),
	); err == nil {
		t.Fatal("New: got nil error, want a rejection of a negative retry bound")
	}
}
