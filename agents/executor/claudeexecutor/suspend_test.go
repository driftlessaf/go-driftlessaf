/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package claudeexecutor_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/claudetool"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const askAFriendToolName = "ask_a_friend"

// askAFriendProvider is a SuspendProvider registering an ask_a_friend tool the model
// can call to pause the conversation for a human answer.
func askAFriendProvider() claudeexecutor.SuspendProvider {
	return func() (anthropic.ToolParam, error) {
		return anthropic.ToolParam{
			Name:        askAFriendToolName,
			Description: anthropic.String("Pause and ask a human operator a question."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type:       "object",
				Properties: map[string]any{"question": map[string]any{"type": "string"}},
			},
		}, nil
	}
}

// toolUseBlockSSE renders one assistant tool_use content block at the given
// stream index, with input streamed as an input_json_delta. Used to compose
// single- and multi-tool suspend turns.
func toolUseBlockSSE(t *testing.T, index int, callID, name string, input map[string]any) []string {
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
		fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, callID, name),
		fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, partial),
		fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index),
	}
}

// wrapTurn wraps content-block SSE events with the message_start / message_delta
// / message_stop envelope of a tool_use turn.
func wrapTurn(msgID string, blocks ...string) []string {
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`, msgID),
	}
	events = append(events, blocks...)
	events = append(events,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)
	return events
}

// TestSuspendReturnsSuspension is the PR5 core case: with WithSuspendTool
// applied, a model turn that calls the suspend tool halts the run — Execute
// returns a *checkpoint.Suspension (an error value) whose envelope carries the
// suspend call as the single pending tool call, keyed by the provider-assigned
// tool_use ID, plus the captured provider state and config digest.
func TestSuspendReturnsSuspension(t *testing.T) {
	const maxTurns = 7
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		return wrapTurn("msg_ask",
			toolUseBlockSSE(t, 0, "toolu_ask", askAFriendToolName,
				map[string]any{"reasoning": "need a human decision", "question": "ship it?"})...)
	})

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](maxTurns),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})
	if err == nil {
		t.Fatal("Execute: got nil error, want a *checkpoint.Suspension")
	}
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("AsSuspension: err %v is not a *checkpoint.Suspension", err)
	}

	if got, want := len(susp.PendingToolCalls), 1; got != want {
		t.Fatalf("PendingToolCalls: got %d, want %d", got, want)
	}
	pc := susp.PendingToolCalls[0]
	if pc.ID != "toolu_ask" {
		t.Errorf("PendingToolCalls[0].ID: got %q, want %q", pc.ID, "toolu_ask")
	}
	if pc.Name != askAFriendToolName {
		t.Errorf("PendingToolCalls[0].Name: got %q, want %q", pc.Name, askAFriendToolName)
	}
	if got, want := susp.Turn, 0; got != want {
		t.Errorf("Turn: got %d, want %d", got, want)
	}
	if got, want := susp.RemainingTurns, maxTurns-1; got != want {
		t.Errorf("RemainingTurns: got %d, want %d", got, want)
	}
	if susp.Provider != "anthropic" {
		t.Errorf("Provider: got %q, want %q", susp.Provider, "anthropic")
	}
	if susp.ConfigDigest == "" {
		t.Error("ConfigDigest: got empty, want a digest")
	}
	if len(susp.ProviderState) == 0 {
		t.Error("ProviderState: got empty, want the serialized request params")
	}
	// The captured provider state must round-trip as a MessageNewParams whose
	// transcript includes the assistant tool_use for the pending call.
	if !strings.Contains(string(susp.ProviderState), "toolu_ask") {
		t.Errorf("ProviderState does not reference the pending tool_use id: %s", susp.ProviderState)
	}
}

// TestSubmitAndSuspendSameTurn pins the submit-wins rule: when one turn issues
// both the terminal submit call and the suspend call, the terminal result makes
// the pending question moot — Execute commits the submitted result and returns
// no error, never a Suspension.
func TestSubmitAndSuspendSameTurn(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		submitJSON, err := json.Marshal(submitInput("done"))
		if err != nil {
			t.Fatalf("marshal submit input: %v", err)
		}
		partial, err := json.Marshal(string(submitJSON))
		if err != nil {
			t.Fatalf("marshal partial: %v", err)
		}
		submitBlock := []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_submit","name":"submit_result","input":{}}}`,
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial),
			`{"type":"content_block_stop","index":0}`,
		}
		askBlock := toolUseBlockSSE(t, 1, "toolu_ask", askAFriendToolName,
			map[string]any{"reasoning": "just in case", "question": "ship it?"})
		return wrapTurn("msg_both", append(submitBlock, askBlock...)...)
	})

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithRetryConfig[errCapRequest, errCapResponse](fastRetry(0)),
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, map[string]claudetool.Metadata[errCapResponse]{})
	if err != nil {
		t.Fatalf("Execute: got err %v, want nil (submit wins over suspend)", err)
	}
	if _, ok := checkpoint.AsSuspension(err); ok {
		t.Fatal("Execute returned a Suspension; submit must win in the same turn")
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got %q, want %q", got, want)
	}
}

// TestWithSuspendToolCollidesWithSubmit proves the construction-time guard: the
// suspend tool name must differ from the submit tool name, checked regardless of
// option order.
func TestWithSuspendToolCollidesWithSubmit(t *testing.T) {
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	collidingProvider := func() (anthropic.ToolParam, error) {
		return anthropic.ToolParam{Name: "submit_result"}, nil
	}
	_, err = claudeexecutor.New[errCapRequest, errCapResponse](
		anthropic.NewClient(option.WithAPIKey("test")),
		prompt,
		claudeexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submitresult.ClaudeToolForResponse[errCapResponse]),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](collidingProvider),
	)
	if err == nil {
		t.Fatal("New: got nil error, want a submit/suspend name-collision error")
	}
	if !strings.Contains(err.Error(), "collides with the submit tool name") {
		t.Errorf("New error: got %q, want a submit-collision message", err.Error())
	}
}

// TestSuspendCallerToolCollisionRejected proves the Execute-time guard: a
// caller-registered tool that shadows the suspend tool name would silently drop
// the pause, so Execute rejects the run.
func TestSuspendCallerToolCollisionRejected(t *testing.T) {
	srv := newValidatingAnthropicServer(t, func(int, []byte) []string {
		return wrapTurn("msg_x", toolUseBlockSSE(t, 0, "toolu_x", askAFriendToolName, map[string]any{"reasoning": "x"})...)
	})
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	exec, err := claudeexecutor.New[errCapRequest, errCapResponse](
		client, prompt,
		claudeexecutor.WithMaxTurns[errCapRequest, errCapResponse](3),
		claudeexecutor.WithSuspendTool[errCapRequest, errCapResponse](askAFriendProvider()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Register a caller tool with the same name as the suspend tool.
	tools := map[string]claudetool.Metadata[errCapResponse]{
		askAFriendToolName: {Definition: anthropic.ToolParam{Name: askAFriendToolName}},
	}
	_, err = exec.Execute(t.Context(), errCapRequest{}, tools)
	if err == nil {
		t.Fatal("Execute: got nil error, want a caller-tool collision error")
	}
	if _, ok := checkpoint.AsSuspension(err); ok {
		t.Fatal("Execute returned a Suspension; a shadowed suspend tool must be rejected, not fired")
	}
	if !strings.Contains(err.Error(), "collides with a caller-registered tool") {
		t.Errorf("Execute error: got %q, want a caller-collision message", err.Error())
	}
}
