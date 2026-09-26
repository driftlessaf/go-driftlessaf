/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func runtimeClaudeRoute() modelrouter.Route {
	route := bedrockChatRoute()
	route.Selection.LogicalModel = "claude-sonnet-5"
	route.Protocol = modelrouter.ProtocolAnthropicMessages
	route.ProviderModelID = "us.anthropic.claude-sonnet-5"
	return route
}

// Replace only the HTTP boundary. The real shared transport's signing,
// credential refresh, redaction, and redirect tests remain in bedrockruntime.
func runtimeClaudeAdapter(t *testing.T, handler http.HandlerFunc) AnthropicMessagesAdapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := awsauth.Config{Region: "us-west-2", Profile: "approved-sso"}
	adapter, err := newBedrockRuntimeAnthropicMessagesAdapter(cfg, func(_ context.Context, got awsauth.Config) (bedrockruntime.Client, error) {
		if got != cfg {
			t.Errorf("AWS config: got = %+v, want = %+v", got, cfg)
		}
		return &bedrockChatTestClient{Client: server.Client(), endpoint: server.URL}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestRuntimeClaudeToolLoop(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_AWS_API_KEY"} {
		t.Setenv(key, rand.Text())
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL"} {
		t.Setenv(key, "https://must-not-be-used.invalid/")
	}
	t.Setenv("CLAUDE_BACKEND", "vertex")
	route := runtimeClaudeRoute()
	calls := 0
	adapter := runtimeClaudeAdapter(t, func(w http.ResponseWriter, request *http.Request) {
		calls++
		if got := request.Method + " " + request.URL.Path; got != "POST /anthropic/v1/messages" {
			t.Errorf("request: got = %q, want = POST /anthropic/v1/messages", got)
		}
		for _, key := range []string{"Authorization", "X-Api-Key", "X-Amz-Security-Token"} {
			if request.Header.Get(key) != "" {
				t.Errorf("unexpected %s before signer", key)
			}
		}
		if got := request.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Errorf("version: got = %q, want = 2023-06-01", got)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != route.ProviderModelID || body["stream"] != true || body["max_tokens"] != float64(256) {
			t.Error("incorrect model, streaming, or output-token limit")
		}
		name, args := "lookup", `{"reasoning":"read fixture"}`
		if calls > 1 {
			messages, err := json.Marshal(body["messages"])
			if err != nil || !strings.Contains(string(messages), "local-test-value") || !strings.Contains(string(messages), `"tool_use_id":"tool-1"`) {
				t.Error("tool result missing from continuation")
			}
			name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
		}
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
			"id": fmt.Sprintf("msg-%d", calls), "type": "message", "role": "assistant", "model": route.ProviderModelID,
			"content": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 1, "cache_read_input_tokens": 2},
		}})
		writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("tool-%d", calls), "name": name, "input": map[string]any{},
		}})
		for _, part := range []string{args[:len(args)/2], args[len(args)/2:]} {
			writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
		}
		writeEvent(map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 3}})
		writeEvent(map[string]any{"type": "message_stop"})
	})
	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{AnthropicMessages: adapters})
	cfg := routedTestConfig(t)
	tools := new(bedrockChatTools)
	cfg.Tools = tools
	cfg.MaxTokens = 256
	cfg.MaxTurns = 2
	agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tracer := new(basetenRecordingTracer)
	result, err := agent.Execute(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer), &testRequest{}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || calls != 2 || tools.calls != 1 {
		t.Fatalf("result/calls/tools: got = %q/%d/%d, want = done/2/1", result.Answer, calls, tools.calls)
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 2 {
		t.Fatal("expected one trace with two turns")
	}
	for _, turn := range tracer.traces[0].Turns {
		if got, want := turn.ServingLocation, "us-west-2"; got != want {
			t.Errorf("serving location: got = %q, want = %q", got, want)
		}
		if turn.Provider != agenttrace.SystemBedrock || turn.System != agenttrace.SystemBedrock || turn.Protocol != string(route.Protocol) || turn.Model != route.ProviderModelID || turn.LogicalModel != route.Selection.LogicalModel {
			t.Errorf("incorrect attribution: %+v", turn)
		}
		if turn.InputTokens != 7 || turn.OutputTokens != 3 || turn.CacheReadTokens != 2 {
			t.Errorf("tokens: got = %d/%d/%d, want = 7/3/2", turn.InputTokens, turn.OutputTokens, turn.CacheReadTokens)
		}
	}
}

func TestRuntimeClaudeServiceErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/stream=%v", status, streaming), func(t *testing.T) {
				t.Parallel()
				calls := 0
				adapter := runtimeClaudeAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
					calls++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"fixture"}}`)
				})
				binding, err := adapter(t.Context(), mustClaudePlan(t, runtimeClaudeRoute()))
				if err != nil {
					t.Fatal(err)
				}
				messages := binding.Messages()
				if !option.HasWithoutEnvironmentDefaults(messages.Options) {
					t.Fatal("SDK environment defaults enabled")
				}
				params := anthropic.MessageNewParams{Model: binding.Plan().ProviderModelID(), MaxTokens: 16,
					Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("fixture"))}}
				if streaming {
					stream := messages.NewStreaming(t.Context(), params)
					defer stream.Close()
					if stream.Next() {
						t.Error("unexpected stream event")
					}
					err = stream.Err()
				} else {
					_, err = messages.New(t.Context(), params)
				}
				if apiErr, ok := errors.AsType[*anthropic.Error](err); !ok || apiErr.StatusCode != status {
					t.Errorf("error: got = %v, want = API status %d", err, status)
				}
				if calls != 1 {
					t.Errorf("HTTP attempts: got = %d, want = 1", calls)
				}
			})
		}
	}
}

func TestRuntimeClaudeInvalidConfig(t *testing.T) {
	t.Parallel()
	if _, err := NewBedrockRuntimeAnthropicMessagesAdapter(awsauth.Config{Region: "us-west-2", Profile: " spaced "}); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("profile: got = %v, want = ErrInvalidAdapter", err)
	}
	if _, err := newBedrockRuntimeAnthropicMessagesAdapter(awsauth.Config{Region: "us-west-2"}, nil); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("factory: got = %v, want = ErrInvalidAdapter", err)
	}
}
