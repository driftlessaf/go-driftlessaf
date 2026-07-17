/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package openaiexecutor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// validatingOpenAIReqBody is the subset of the chat-completions request the
// validating server inspects: the message transcript with enough of each
// message to check assistant(tool_calls) → tool(tool_call_id) pairing.
type validatingOpenAIReqBody struct {
	Messages []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

// openAIPairingViolations parses a chat-completions request body and returns a
// message for every tool_call/tool pairing violation: every tool_call
// advertised on an assistant message must be answered by a role:"tool" message
// carrying the matching tool_call_id in the run of tool messages that
// immediately follows. This is exactly the shape a verbatim transcript replay
// (the DEV-2247 resume risk) would violate. A nil return means the body is
// well-paired. A parse error is reported as a single violation.
func openAIPairingViolations(body []byte) []string {
	var parsed validatingOpenAIReqBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return []string{fmt.Sprintf("invalid JSON body: %v", err)}
	}

	var violations []string
	for i, m := range parsed.Messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		// Collect the tool_call_ids answered by the run of role:"tool"
		// messages immediately following this assistant message. OpenAI
		// appends one tool message per tool_call directly after the
		// assistant turn.
		answered := make(map[string]struct{})
		for j := i + 1; j < len(parsed.Messages) && parsed.Messages[j].Role == "tool"; j++ {
			answered[parsed.Messages[j].ToolCallID] = struct{}{}
		}
		for _, tc := range m.ToolCalls {
			if _, ok := answered[tc.ID]; !ok {
				violations = append(violations, fmt.Sprintf(
					"tool_call %q on assistant message %d has no matching tool message with that tool_call_id",
					tc.ID, i))
			}
		}
	}
	return violations
}

// newValidatingOpenAIServer returns an httptest server that stands in for the
// OpenAI-compatible chat-completions API. For each request it parses the body
// and asserts the tool_call/tool message pairing the real API enforces (see
// openAIPairingViolations) before returning a scripted completion.
//
// script returns the completion JSON to return for the given 1-based request
// number; body is the raw request body so a script can vary its response on
// what the model was sent. Assertion failures are reported with t.Errorf (safe
// from the handler goroutine) so the driving test fails without aborting the
// in-flight HTTP exchange.
func newValidatingOpenAIServer(t *testing.T, script func(reqNum int, body []byte) string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var reqNum int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqNum++
		n := reqNum
		mu.Unlock()

		for _, v := range openAIPairingViolations(body) {
			t.Errorf("request %d: %s", n, v)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, script(n, body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestValidatingOpenAIServerAcceptsPairedTranscript proves the validating
// server helper works end-to-end: it drives a two-tool-call turn followed by a
// submit turn, so the third request replays two well-formed
// assistant(tool_calls) → tool(tool_call_id) pairs, and the run completes
// without the server flagging a pairing violation.
func TestValidatingOpenAIServerAcceptsPairedTranscript(t *testing.T) {
	srv := newValidatingOpenAIServer(t, func(reqNum int, _ []byte) string {
		if reqNum == 1 {
			return turn1ToolCallsJSON
		}
		return turn2SubmitJSON
	})

	client := openai.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)

	prompt, err := promptbuilder.NewPrompt("go")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}

	mkTool := func(name string) openaistool.Metadata[errCapResponse] {
		return openaistool.FromTool(toolcall.Tool[errCapResponse]{
			Def: toolcall.Definition{Name: name, Description: "test tool " + name},
			Handler: func(context.Context, toolcall.ToolCall, *agenttrace.Trace[errCapResponse], *errCapResponse) map[string]any {
				return map[string]any{"ok": name}
			},
		})
	}
	tools := map[string]openaistool.Metadata[errCapResponse]{
		"tool_a": mkTool("tool_a"),
		"tool_b": mkTool("tool_b"),
	}

	submit := func() (openaistool.SubmitMetadata[errCapResponse], error) {
		return openaistool.SubmitMetadata[errCapResponse]{
			Definition: openai.ChatCompletionToolParam{
				Function: shared.FunctionDefinitionParam{Name: "submit_result"},
			},
			Handler: func(context.Context, openai.ChatCompletionMessageToolCall, *agenttrace.Trace[errCapResponse]) toolcall.SubmitOutcome[errCapResponse] {
				return toolcall.SubmitOutcome[errCapResponse]{
					Accepted:   true,
					Response:   errCapResponse{Answer: "done"},
					ToolResult: map[string]any{"success": true},
				}
			},
		}, nil
	}

	exec, err := openaiexecutor.New[errCapRequest, errCapResponse](
		client,
		prompt,
		openaiexecutor.WithSubmitResultProvider[errCapRequest, errCapResponse](submit),
		openaiexecutor.WithMaxTurns[errCapRequest, errCapResponse](5),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := exec.Execute(t.Context(), errCapRequest{}, tools)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := resp.Answer, "done"; got != want {
		t.Errorf("resp.Answer: got = %q, want = %q", got, want)
	}
}

// TestOpenAIPairingViolationsRejectsUnpairedTranscript proves the pairing
// check actually fires: an assistant message advertising a tool_call with no
// following tool message must be flagged. Without this the validating server
// would be a blind mock that never catches a broken transcript.
func TestOpenAIPairingViolationsRejectsUnpairedTranscript(t *testing.T) {
	unpaired := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"tool_a","arguments":"{}"}}]}
	]}`)
	if got := openAIPairingViolations(unpaired); len(got) == 0 {
		t.Error("openAIPairingViolations accepted an assistant tool_call with no matching tool message")
	}
}
