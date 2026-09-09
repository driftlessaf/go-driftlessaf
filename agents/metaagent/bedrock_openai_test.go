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
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"chainguard.dev/driftlessaf/agents/toolcall/callbacks"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go"
)

// These tests exercise the real SDK and executor against an HTTP server, not
// AWS authentication. bedrockruntime's tests independently exercise the real
// AWS signer, endpoint isolation, and refreshable credential provider.
type bedrockChatTestClient struct {
	*http.Client
	endpoint string
}

var _ bedrockruntime.Client = (*bedrockChatTestClient)(nil)

func (c *bedrockChatTestClient) Endpoint() string { return c.endpoint }

func bedrockChatRoute() modelrouter.Route {
	// This is a protocol fixture, not a claim of account/model availability.
	return modelrouter.Route{
		Selection: modelrouter.Selection{
			Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "gpt-5.6-terra",
		},
		Protocol:        modelrouter.ProtocolOpenAIChatCompletions,
		ProviderModelID: "us.openai.gpt-5.6-terra",
		Attribution: modelrouter.Attribution{
			ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock,
		},
		Capabilities: modelrouter.Capabilities{
			ToolCalling: true, TerminalSubmission: true, MaximumOutputTokens: true,
		},
	}
}

func bedrockChatPlan(t *testing.T, route modelrouter.Route) modelrouter.Plan {
	t.Helper()
	plan, err := mustRouteRegistry(t, route).Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return plan
}

func bedrockChatAdapter(t *testing.T, handler http.HandlerFunc) OpenAIChatCompletionsAdapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := awsauth.Config{Region: "us-east-1", Profile: "test-sso"}
	adapter, err := newBedrockOpenAIChatCompletionsAdapter(cfg, func(_ context.Context, got awsauth.Config) (bedrockruntime.Client, error) {
		if diff := cmp.Diff(cfg, got); diff != "" {
			t.Errorf("AWS config mismatch (-want +got):\n%s", diff)
		}
		return &bedrockChatTestClient{Client: server.Client(), endpoint: server.URL}, nil
	})
	if err != nil {
		t.Fatalf("newBedrockOpenAIChatCompletionsAdapter: %v", err)
	}
	return adapter
}

func checkBedrockChatRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if got, want := r.Method+" "+r.URL.Path, "POST /openai/v1/chat/completions"; got != want {
		t.Errorf("request: got = %q, want = %q", got, want)
	}
	for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project", "X-Amz-Security-Token"} {
		if r.Header.Get(header) != "" {
			t.Errorf("unexpected %s header before the signing transport", header)
		}
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decoding request: %v", err)
		return nil
	}
	if got, want := body["model"], bedrockChatRoute().ProviderModelID; got != want {
		t.Errorf("model: got = %v, want = %v", got, want)
	}
	if got := body["store"]; got != false {
		t.Errorf("store: got = %v, want = false", got)
	}
	return body
}

func TestBedrockChatTextIgnoresOpenAIEnvironment(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "OPENAI_WEBHOOK_SECRET"} {
		t.Setenv(name, rand.Text())
	}
	t.Setenv("OPENAI_BASE_URL", "https://must-not-be-used.invalid/")
	adapter := bedrockChatAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		checkBedrockChatRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"text","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	})
	plan := bedrockChatPlan(t, bedrockChatRoute())
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if !binding.Plan().SameResolution(plan) {
		t.Error("binding changed the resolved plan")
	}
	if got, want := binding.TokenLimitParameter(), openaiexecutor.TokenLimitMaxCompletionTokens; got != want {
		t.Errorf("token parameter: got = %q, want = %q", got, want)
	}
	client := binding.Client()
	response, err := client.Chat.Completions.New(t.Context(), openai.ChatCompletionNewParams{
		Model: plan.ProviderModelID(), Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Content != "hello" {
		t.Errorf("choices: got = %+v, want = one hello message", response.Choices)
	}
	if got, want := response.Usage.TotalTokens, int64(10); got != want {
		t.Errorf("total tokens: got = %d, want = %d", got, want)
	}
}

func TestBedrockChatStreamsContentAndToolArguments(t *testing.T) {
	adapter := bedrockChatAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		body := checkBedrockChatRequest(t, r)
		if got := body["stream"]; got != true {
			t.Errorf("stream: got = %v, want = true", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"stream","choices":[{"index":0,"delta":{"role":"assistant","content":"checking "}}]}`,
			`{"id":"stream","choices":[{"index":0,"delta":{"content":"now","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"key\":"}}]}}]}`,
			`{"id":"stream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"value\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"stream","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			"[DONE]",
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
	})
	plan := bedrockChatPlan(t, bedrockChatRoute())
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	client := binding.Client()
	stream := client.Chat.Completions.NewStreaming(t.Context(), openai.ChatCompletionNewParams{
		Model: plan.ProviderModelID(), Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("lookup")},
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	defer stream.Close()
	var acc openai.ChatCompletionAccumulator
	for stream.Next() {
		if !acc.AddChunk(stream.Current()) {
			t.Fatal("accumulator rejected a stream chunk")
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(acc.Choices) != 1 {
		t.Fatalf("choices: got = %d, want = 1", len(acc.Choices))
	}
	message := acc.Choices[0].Message
	if got, want := message.Content, "checking now"; got != want {
		t.Errorf("content: got = %q, want = %q", got, want)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls: got = %d, want = 1", len(message.ToolCalls))
	}
	if got, want := message.ToolCalls[0].Function.Arguments, `{"key":"value"}`; got != want {
		t.Errorf("tool arguments: got = %q, want = %q", got, want)
	}
	if got, want := acc.Usage.TotalTokens, int64(10); got != want {
		t.Errorf("total tokens: got = %d, want = %d", got, want)
	}
}

func TestBedrockChatErrorsDoNotRetryInsideSDK(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/stream=%t", status, streaming), func(t *testing.T) {
				calls := 0
				adapter := bedrockChatAdapter(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					checkBedrockChatRequest(t, r)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"test error","type":"api_error"}}`)
				})
				plan := bedrockChatPlan(t, bedrockChatRoute())
				binding, err := adapter(t.Context(), plan)
				if err != nil {
					t.Fatalf("adapter: %v", err)
				}
				client := binding.Client()
				params := openai.ChatCompletionNewParams{Model: plan.ProviderModelID(), Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")}}
				if streaming {
					stream := client.Chat.Completions.NewStreaming(t.Context(), params)
					defer stream.Close()
					if stream.Next() {
						t.Error("stream returned a chunk for an HTTP error")
					}
					err = stream.Err()
				} else {
					_, err = client.Chat.Completions.New(t.Context(), params)
				}
				if apiErr, ok := errors.AsType[*openai.Error](err); !ok || apiErr.StatusCode != status {
					t.Errorf("error: got = %v, want = API error with status %d", err, status)
				}
				if calls != 1 {
					t.Errorf("HTTP calls: got = %d, want = 1 (executor owns retries)", calls)
				}
			})
		}
	}
}

func TestBedrockChatInvalidConfig(t *testing.T) {
	for _, cfg := range []awsauth.Config{{}, {Region: " us-east-1"}, {Region: "us-east-1/path"}, {Region: "US-EAST-1"}, {Region: "us-east-1", Profile: " trailing "}} {
		if _, err := NewBedrockOpenAIChatCompletionsAdapter(cfg); !errors.Is(err, ErrInvalidAdapter) {
			t.Errorf("constructor(%+v): got = %v, want = ErrInvalidAdapter", cfg, err)
		}
	}
	if _, err := newBedrockOpenAIChatCompletionsAdapter(awsauth.Config{Region: "us-east-1"}, nil); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("nil factory: got = %v, want = ErrInvalidAdapter", err)
	}
}

func TestBedrockChatFactoryIsLazyAndPropagatesFailure(t *testing.T) {
	calls := 0
	wantErr := errors.New("test credential validation failure")
	adapter, err := newBedrockOpenAIChatCompletionsAdapter(awsauth.Config{Region: "us-east-1"}, func(context.Context, awsauth.Config) (bedrockruntime.Client, error) {
		calls++
		return nil, wantErr
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if calls != 0 {
		t.Errorf("factory calls at construction: got = %d, want = 0", calls)
	}
	route := bedrockChatRoute()
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{Provider: route.Selection.Provider, Adapter: adapter})
	if err != nil {
		t.Fatalf("adapter registry: %v", err)
	}
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIChatCompletions: adapters})
	config := routedTestConfig(t)
	config.ThinkingBudget = 1024
	if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, config); !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Errorf("unsupported capability: got = %v, want = ErrUnsupportedCapability", err)
	}
	if calls != 0 {
		t.Errorf("factory calls for unsupported capability: got = %d, want = 0", calls)
	}
	if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, routedTestConfig(t)); !errors.Is(err, wantErr) {
		t.Errorf("NewRouted: got = %v, want = %v", err, wantErr)
	}
	if calls != 1 {
		t.Errorf("factory calls: got = %d, want = 1 (no fallback)", calls)
	}
}

func TestBedrockChatCancellationReachesHTTPClient(t *testing.T) {
	started := make(chan struct{})
	adapter := bedrockChatAdapter(t, func(_ http.ResponseWriter, r *http.Request) {
		checkBedrockChatRequest(t, r)
		close(started)
		<-r.Context().Done()
	})
	plan := bedrockChatPlan(t, bedrockChatRoute())
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		client := binding.Client()
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: plan.ProviderModelID(), Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
		})
		done <- err
	}()
	select {
	case <-started:
	case <-t.Context().Done():
		t.Fatal("request did not reach the HTTP client")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("completion: got = %v, want = context.Canceled", err)
	}
}

type bedrockChatTools struct{ calls int }

var _ toolcall.ToolProvider[*basetenTestResponse, toolcall.EmptyTools] = (*bedrockChatTools)(nil)

func (b *bedrockChatTools) Tools(context.Context, toolcall.EmptyTools) (map[string]toolcall.Tool[*basetenTestResponse], error) {
	return map[string]toolcall.Tool[*basetenTestResponse]{
		"lookup": {
			Def: toolcall.Definition{Name: "lookup", Description: "Read a local test value"},
			Handler: func(_ context.Context, call toolcall.ToolCall, trace *agenttrace.Trace[*basetenTestResponse], _ **basetenTestResponse) map[string]any {
				b.calls++
				return map[string]any{"value": "local-test-value"}
			},
		},
	}, nil
}

func TestBedrockChatRoutedToolLoopAndAttribution(t *testing.T) {
	turns := 0
	adapter := bedrockChatAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		turns++
		body := checkBedrockChatRequest(t, r)
		if got := body["max_completion_tokens"]; got != float64(256) {
			t.Errorf("max_completion_tokens: got = %v, want = 256", got)
		}
		for _, name := range []string{"max_tokens", "temperature", "top_p", "reasoning_effort"} {
			if _, ok := body[name]; ok {
				t.Errorf("unexpected request parameter %q", name)
			}
		}
		name, args := "lookup", `{"reasoning":"read the fixture"}`
		if turns > 1 {
			messages, err := json.Marshal(body["messages"])
			if err != nil || !strings.Contains(string(messages), "local-test-value") || !strings.Contains(string(messages), `"tool_call_id":"call-1"`) {
				t.Errorf("tool result missing from next request: %s (error %v)", messages, err)
			}
			name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "completion", "object": "chat.completion", "model": bedrockChatRoute().ProviderModelID,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "tool_calls": []any{map[string]any{
					"id": fmt.Sprintf("call-%d", turns), "type": "function", "function": map[string]any{"name": name, "arguments": args},
				}},
			}}},
			"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
		})
	})
	route := bedrockChatRoute()
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{Provider: route.Selection.Provider, Adapter: adapter})
	if err != nil {
		t.Fatalf("adapter registry: %v", err)
	}
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIChatCompletions: adapters})
	config := routedTestConfig(t)
	tools := &bedrockChatTools{}
	config.Tools = tools
	config.MaxTurns = 2
	config.MaxTokens = 256
	config.ToolCallConcurrency = 1
	config.ResultValidators = []callbacks.ResultValidator[*basetenTestResponse]{
		func(context.Context, *basetenTestResponse, string) ([]callbacks.Finding, error) { return nil, nil },
	}
	agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, config)
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}
	tracer := &basetenRecordingTracer{}
	result, err := agent.Execute(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer), &testRequest{}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Answer != "done" || tools.calls != 1 || turns != 2 {
		t.Errorf("result/calls/turns: got = %q/%d/%d, want = done/1/2", result.Answer, tools.calls, turns)
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 2 {
		t.Fatalf("traces: got = %+v, want = one trace with two turns", tracer.traces)
	}
	for _, turn := range tracer.traces[0].Turns {
		if turn.Model != route.ProviderModelID || turn.LogicalModel != route.Selection.LogicalModel || turn.Provider != agenttrace.SystemBedrock || turn.System != agenttrace.SystemBedrock || turn.Protocol != string(route.Protocol) {
			t.Errorf("incorrect routed attribution: %+v", turn)
		}
		if turn.InputTokens != 7 || turn.OutputTokens != 3 {
			t.Errorf("tokens: got = %d/%d, want = 7/3", turn.InputTokens, turn.OutputTokens)
		}
	}
}

func TestLegacyConstructorStillRejectsBareGPT(t *testing.T) {
	_, err := New[*testRequest](t.Context(), "project", "us-central1", "gpt-5.6-terra", routedTestConfig(t))
	if err == nil || !strings.Contains(err.Error(), "unsupported model:") {
		t.Errorf("New: got = %v, want = unsupported model without credential discovery", err)
	}
}
