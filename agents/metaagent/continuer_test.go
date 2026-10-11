/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	agentexecutor "chainguard.dev/driftlessaf/agents/executor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestClaudeAgentIsContinuer proves the Claude backend exposes the Continuer
// capability through AsContinuer. Construction acquires Application Default
// Credentials (the Vertex path), so the test points
// GOOGLE_APPLICATION_CREDENTIALS at a hermetic fake key.
func TestClaudeAgentIsContinuer(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))

	agent, err := New[*testRequest](t.Context(), "test-project", "us-central1", "claude-test-model", routedTestConfig(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := AsContinuer[*testRequest](agent); !ok {
		t.Error("AsContinuer() = false for a Claude-backed agent, want true")
	}
}

// TestAsContinuerReportsFalseForExecuteOnlyAgent pins AsContinuer's fallback
// contract: an Agent without the Continue method reports false, so callers
// run from scratch instead.
func TestAsContinuerReportsFalseForExecuteOnlyAgent(t *testing.T) {
	var agent Agent[*testRequest, *testResponse, testCallbacks] = executeOnlyAgent{}
	if _, ok := AsContinuer[*testRequest](agent); ok {
		t.Error("AsContinuer() = true for an Execute-only agent, want false")
	}
}

// TestClaudeAgentContinuesAnExhaustedRun drives two routed agents built from
// one Config that differ only in MaxTurns: the 2-turn agent spends its budget
// on lookups, and the 4-turn agent continues that conversation instead of
// starting over. Its first request carries both earlier tool results, it
// submits on the conversation's third turn, and the tools ran once per
// lookup the model made, never again.
func TestClaudeAgentContinuesAnExhaustedRun(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body struct {
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		name, args := "lookup", `{"reasoning":"read fixture"}`
		if n == 3 {
			for _, id := range []string{"tool-1", "tool-2"} {
				if !strings.Contains(string(body.Messages), fmt.Sprintf(`"tool_use_id":%q`, id)) {
					t.Errorf("continued request lacks the result of %s: the exhausted transcript was not carried", id)
				}
			}
			name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
		}
		writeToolUseTurn(t, w, n, name, args)
	}))
	t.Cleanup(srv.Close)

	selection := modelrouter.Selection{Provider: "scripted", LogicalModel: "claude-sonnet-4-6"}
	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{
		Provider: selection.Provider,
		Adapter: func(_ context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("scripted"), option.WithMaxRetries(0))
			return NewAnthropicMessagesBinding(plan, client.Messages, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")),
		AdapterRegistries{AnthropicMessages: adapters})

	tools := new(bedrockChatTools)
	agentAt := func(maxTurns int) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
		cfg := routedTestConfig(t)
		cfg.Tools = tools
		cfg.MaxTurns = maxTurns
		agent, err := NewRouted[*testRequest](t.Context(), router, selection, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}

	ctx := agenttrace.WithTracer[*basetenTestResponse](t.Context(), new(basetenRecordingTracer))
	_, err = agentAt(2).Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
	exhausted, ok := errors.AsType[*agentexecutor.MaxTurnsError](err)
	if !ok || exhausted.Continuation == nil {
		t.Fatalf("Execute: got %v, want a turn exhaustion carrying its conversation", err)
	}

	continuer, ok := AsContinuer(agentAt(4))
	if !ok {
		t.Fatal("AsContinuer() = false for a Claude-backed agent")
	}
	result, err := continuer.Continue(ctx, *exhausted.Continuation, toolcall.EmptyTools{})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if result.Answer != "done" || requests.Load() != 3 || tools.calls != 2 {
		t.Fatalf("result/requests/tool calls: got = %q/%d/%d, want = done/3/2", result.Answer, requests.Load(), tools.calls)
	}
}

// writeToolUseTurn streams one assistant turn that calls name with args.
func writeToolUseTurn(t *testing.T, w http.ResponseWriter, n int32, name, args string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	writeEvent := func(event map[string]any) {
		data, err := json.Marshal(event)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
	writeEvent(map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg-%d", n), "type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
		"content": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 1},
	}})
	writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{
		"type": "tool_use", "id": fmt.Sprintf("tool-%d", n), "name": name, "input": map[string]any{},
	}})
	writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
	writeEvent(map[string]any{"type": "content_block_stop", "index": 0})
	writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 3}})
	writeEvent(map[string]any{"type": "message_stop"})
}
