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
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
)

// This boundary double supplies only HTTP. Real signing, token refresh, origin
// restriction, and redirect denial remain covered by bedrockruntime's tests.
func bedrockResponsesAdapter(t *testing.T, handler http.HandlerFunc) OpenAIResponsesAdapter {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	cfg := awsauth.Config{Region: "us-west-2", Profile: "test-sso"}
	adapter, err := newBedrockOpenAIResponsesAdapter(cfg, func(_ context.Context, got awsauth.Config) (bedrockruntime.Client, error) {
		if diff := cmp.Diff(cfg, got); diff != "" {
			t.Errorf("AWS config (-want +got): %s", diff)
		}
		return &bedrockChatTestClient{Client: s.Client(), endpoint: s.URL}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestBedrockResponsesRoutedContinuation(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "OPENAI_WEBHOOK_SECRET"} {
		t.Setenv(name, rand.Text())
	}
	t.Setenv("OPENAI_BASE_URL", "https://must-not-be-used.invalid/")
	route := responsesRoute()
	n := 0
	adapter := bedrockResponsesAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if got, want := r.Method+" "+r.URL.Path, "POST /openai/v1/responses"; got != want {
			t.Errorf("path: got=%q want=%q", got, want)
		}
		for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project", "X-Amz-Security-Token"} {
			if r.Header.Get(header) != "" {
				t.Errorf("unexpected %s before signer", header)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["model"] != route.ProviderModelID || body["store"] != false || body["stream"] != true || body["max_output_tokens"] != float64(256) {
			t.Error("incorrect model or request controls")
		}
		for _, key := range []string{"messages", "previous_response_id", "temperature", "max_completion_tokens"} {
			if _, ok := body[key]; ok {
				t.Errorf("unexpected %s", key)
			}
		}
		name, args := "lookup", `{"reasoning":"read fixture"}`
		if n > 1 {
			items, err := json.Marshal(body["input"])
			if err != nil || !strings.Contains(string(items), "local-test-value") || !strings.Contains(string(items), `"call_id":"call-1"`) {
				t.Error("native continuation missing tool output")
			}
			name, args = "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": fmt.Sprintf("response-%d", n), "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc-%d", n), "call_id": fmt.Sprintf("call-%d", n), "name": name, "arguments": args, "status": "completed"}},
			"usage":  map[string]any{"input_tokens": 7, "output_tokens": 3, "input_tokens_details": map[string]any{"cached_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}},
		}})
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
	})
	registry, err := NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{Provider: route.Selection.Provider, Adapter: adapter})
	if err != nil {
		t.Fatal(err)
	}
	router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry})
	cfg := routedTestConfig(t)
	cfg.MaxTurns = 2
	cfg.MaxTokens = 256
	tools := new(bedrockChatTools)
	cfg.Tools = tools
	agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tracer := new(basetenRecordingTracer)
	result, err := agent.Execute(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer), &testRequest{}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || tools.calls != 1 || n != 2 {
		t.Fatalf("result=%v tool calls=%d requests=%d", result, tools.calls, n)
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 2 {
		t.Fatal("missing traces")
	}
	for _, turn := range tracer.traces[0].Turns {
		if got, want := turn.ServingLocation, "us-west-2"; got != want {
			t.Errorf("serving location: got = %q, want = %q", got, want)
		}
		if turn.Provider != agenttrace.SystemBedrock || turn.Protocol != string(route.Protocol) || turn.Model != route.ProviderModelID || turn.LogicalModel != route.Selection.LogicalModel || turn.ReasoningTokens != 1 || turn.CacheReadTokens != 2 {
			t.Errorf("turn=%+v", turn)
		}
	}
}

func TestBedrockResponsesConfigAndLazyFailure(t *testing.T) {
	t.Parallel()
	for _, cfg := range []awsauth.Config{{}, {Region: " us-west-2"}, {Region: "us-west-2/escape"}, {Region: "us-west-2", Profile: " spaced "}} {
		if _, err := NewBedrockOpenAIResponsesAdapter(cfg); !errors.Is(err, ErrInvalidAdapter) {
			t.Error(err)
		}
	}
	if _, err := newBedrockOpenAIResponsesAdapter(awsauth.Config{Region: "us-west-2"}, nil); !errors.Is(err, ErrInvalidAdapter) {
		t.Error(err)
	}
	calls := 0
	want := errors.New("fixture credential failure")
	adapter, err := newBedrockOpenAIResponsesAdapter(awsauth.Config{Region: "us-west-2"}, func(context.Context, awsauth.Config) (bedrockruntime.Client, error) { calls++; return nil, want })
	if err != nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if _, err := adapter(t.Context(), bedrockChatPlan(t, responsesRoute())); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestBedrockResponsesServiceDoesNotRetry(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			t.Parallel()
			calls := 0
			adapter := bedrockResponsesAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"fixture"}}`)
			})
			plan := bedrockChatPlan(t, responsesRoute())
			binding, err := adapter(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			service := binding.Responses()
			params := responses.ResponseNewParams{Model: plan.ProviderModelID(), Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("synthetic")}}
			if streaming {
				stream := service.NewStreaming(t.Context(), params)
				defer stream.Close()
				if stream.Next() {
					t.Error("unexpected stream event")
				}
				err = stream.Err()
			} else {
				_, err = service.New(t.Context(), params)
			}
			if e, ok := errors.AsType[*openai.Error](err); !ok || e.StatusCode != http.StatusTooManyRequests || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
