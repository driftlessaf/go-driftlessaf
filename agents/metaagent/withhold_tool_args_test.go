/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chainguard-dev/clog"
	"google.golang.org/genai"

	"chainguard.dev/driftlessaf/agents/executor/openai/chatcompletionexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
)

// withholdTestAgent builds a single-shot agent whose model submits args on its
// first turn.
type withholdTestAgent func(t *testing.T, args string, withhold bool) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools]

// Config.WithholdToolArgs reaches each provider's executor: a single-shot call
// logs its submitted answer without it and withholds it with it.
func TestWithholdToolArgs(t *testing.T) {
	const secret = "submitted-answer-5521"
	raw, err := json.Marshal(map[string]any{"reasoning": "r", "result": map[string]any{"answer": secret}})
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)

	for _, provider := range []struct {
		name     string
		newAgent withholdTestAgent
	}{
		{name: "openai-compatible", newAgent: newOpenAICompatibleWithholdAgent},
		{name: "claude", newAgent: newClaudeWithholdAgent},
		{name: "google", newAgent: newGoogleWithholdAgent},
	} {
		for _, tc := range []struct {
			name     string
			withhold bool
		}{
			{name: "unset logs the answer", withhold: false},
			{name: "set withholds it", withhold: true},
		} {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				a := provider.newAgent(t, args, tc.withhold)
				var logs bytes.Buffer
				ctx := clog.WithLogger(t.Context(), clog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
				got, err := a.Execute(ctx, &testRequest{}, toolcall.EmptyTools{})
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || got.Answer != secret {
					t.Fatalf("result: got = %+v, want the submitted answer", got)
				}
				var line string
				for l := range strings.SplitSeq(logs.String(), "\n") {
					if strings.Contains(l, "Executing tool call") {
						line = l
					}
				}
				if line == "" {
					t.Fatalf("no tool-call line logged:\n%s", logs.String())
				}
				if logged := strings.Contains(line, secret); logged == tc.withhold {
					t.Errorf("answer logged: got = %t, want = %t\n%s", logged, !tc.withhold, line)
				}
				// Both submit arguments, reasoning and result, are withheld.
				if counted := strings.Contains(line, `"args_withheld":2`); counted != tc.withhold {
					t.Errorf("args_withheld=2: got = %t, want = %t\n%s", counted, tc.withhold, line)
				}
			})
		}
	}
}

func withholdTestConfig(withhold bool) Config[*basetenTestResponse, toolcall.EmptyTools] {
	return Config[*basetenTestResponse, toolcall.EmptyTools]{
		UserPrompt:       promptbuilder.MustNewPrompt("answer"),
		Tools:            toolcall.NewEmptyToolsProvider[*basetenTestResponse](),
		WithholdToolArgs: withhold,
	}
}

func newOpenAICompatibleWithholdAgent(t *testing.T, args string, withhold bool) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "s1", "type": "function",
					"function": map[string]any{"name": "submit_result", "arguments": args}}},
			}}},
			"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6},
		})
	}))
	t.Cleanup(srv.Close)
	a, err := NewOpenAICompatible[*testRequest](OpenAICompatibleProvider{
		BaseURL: srv.URL + "/v1", APIKey: "k", Provider: chatcompletionexecutor.ProviderBaseten,
		TokenLimitParameter: chatcompletionexecutor.TokenLimitMaxTokens,
	}, "m", withholdTestConfig(withhold))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newClaudeWithholdAgent(t *testing.T, args string, withhold bool) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
	t.Helper()
	route := runtimeClaudeRoute()
	adapter := runtimeClaudeAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
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
			"id": "msg-1", "type": "message", "role": "assistant", "model": route.ProviderModelID,
			"content": []any{}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 1},
		}})
		writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": "s1", "name": "submit_result", "input": map[string]any{},
		}})
		writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
		writeEvent(map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 2}})
		writeEvent(map[string]any{"type": "message_stop"})
	})
	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{AnthropicMessages: adapters})
	a, err := NewRouted[*testRequest](t.Context(), router, route.Selection, withholdTestConfig(withhold))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newGoogleWithholdAgent(t *testing.T, args string, withhold bool) Agent[*testRequest, *basetenTestResponse, toolcall.EmptyTools] {
	t.Helper()
	response, err := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{
			map[string]any{"functionCall": map[string]any{"name": "submit_result", "args": json.RawMessage(args)}},
		}}}},
		"usageMetadata": map[string]any{"promptTokenCount": 4, "candidatesTokenCount": 2, "totalTokenCount": 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "cachedContents") {
			_, _ = io.WriteString(w, `{"name":"cachedContents/c","expireTime":"2099-01-01T00:00:00Z"}`)
			return
		}
		_, _ = w.Write(response)
	}))
	t.Cleanup(srv.Close)

	selection := modelrouter.Selection{Provider: "scripted", LogicalModel: "gemini-2.5-flash"}
	adapters, err := NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{
		Provider: selection.Provider,
		Adapter: func(ctx context.Context, plan modelrouter.Plan) (GoogleGenAIBinding, error) {
			client, err := genai.NewClient(ctx, &genai.ClientConfig{
				Backend:     genai.BackendGeminiAPI,
				APIKey:      "scripted",
				HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
			})
			if err != nil {
				return GoogleGenAIBinding{}, err
			}
			return NewGoogleGenAIBinding(plan, client, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")),
		AdapterRegistries{GoogleGenAI: adapters})
	a, err := NewRouted[*testRequest](t.Context(), router, selection, withholdTestConfig(withhold))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
