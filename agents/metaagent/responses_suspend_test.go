/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/checkpoint"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// responsesSuspendRouter routes openAIDirectRoute, with suspend/resume
// declared, to a scripted Responses server. Each request is answered with the
// next function call in calls and its body is recorded.
func responsesSuspendRouter(t *testing.T, calls ...map[string]any) (*Router, modelrouter.Route, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, body)
		mu.Unlock()
		if n >= len(calls) {
			t.Errorf("unexpected request %d", n)
			http.Error(w, "unscripted", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": fmt.Sprintf("resp-%d", n), "status": "completed",
			"output": []any{calls[n]},
			"usage":  map[string]any{"input_tokens": 3, "output_tokens": 1},
		}})
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
	}))
	t.Cleanup(server.Close)
	registry, err := NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{Provider: modelrouter.ProviderOpenAI, Adapter: func(_ context.Context, plan modelrouter.Plan) (OpenAIResponsesBinding, error) {
		return NewOpenAIResponsesBinding(plan, responses.NewResponseService(option.WithBaseURL(server.URL), option.WithHTTPClient(server.Client()), option.WithMaxRetries(0)), nil)
	}})
	if err != nil {
		t.Fatal(err)
	}
	route := openAIDirectRoute()
	route.Capabilities.SuspendResume = true
	return mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry}), route, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

func responsesCall(id, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc-" + id, "call_id": id, "name": name, "arguments": args, "status": "completed"}
}

// TestResponsesAgentParksAndResumesWithResults drives a routed Responses
// agent with a suspend tool: the model parks, ResumeWithResults sends the
// caller's reply as the park call's output, and the resumed conversation
// submits.
func TestResponsesAgentParksAndResumesWithResults(t *testing.T) {
	t.Parallel()
	router, route, bodies := responsesSuspendRouter(t,
		responsesCall("park-1", "park", `{"question":"which branch?"}`),
		responsesCall("submit-1", "submit_result", `{"reasoning":"done","result":{"answer":"main"}}`),
	)
	cfg := routedTestConfig(t)
	cfg.SuspendToolName, cfg.SuspendToolDescription = "park", "Ask the caller."
	agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := AsResumer[*testRequest](agent); !ok {
		t.Error("AsResumer() = false for a Responses-backed agent, want true")
	}
	resumer, ok := AsResultResumer[*testRequest](agent)
	if !ok {
		t.Fatal("AsResultResumer() = false for a Responses-backed agent, want true")
	}
	_, err = agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{})
	susp, ok := checkpoint.AsSuspension(err)
	if !ok {
		t.Fatalf("Execute: got = %v, want a suspension", err)
	}
	if susp.Question != "which branch?" || len(susp.PendingToolCalls) != 1 || susp.PendingToolCalls[0].ID != "park-1" {
		t.Fatalf("suspension: got = %+v", susp.Envelope)
	}
	tools, _ := bodies()[0]["tools"].([]any)
	var advertised bool
	for _, tool := range tools {
		if tool.(map[string]any)["name"] == "park" {
			advertised = true
		}
	}
	if !advertised {
		t.Error("suspend tool not advertised")
	}

	got, err := resumer.ResumeWithResults(t.Context(), susp.Envelope, map[string]string{"park-1": "use main"}, toolcall.EmptyTools{})
	if err != nil {
		t.Fatalf("ResumeWithResults: %v", err)
	}
	if got.Answer != "main" {
		t.Errorf("answer: got = %q, want = %q", got.Answer, "main")
	}
	input, _ := bodies()[1]["input"].([]any)
	last := input[len(input)-1].(map[string]any)
	if last["type"] != "function_call_output" || last["call_id"] != "park-1" || last["output"] != "use main" {
		t.Errorf("resumed input tail: got = %v, want the reply as park-1's output", last)
	}
}

func TestResponsesAgentRejectsSuspendSubmitCollision(t *testing.T) {
	t.Parallel()
	router, route, _ := responsesSuspendRouter(t)
	cfg := routedTestConfig(t)
	cfg.SuspendToolName = "submit_result"
	if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg); err == nil || !strings.Contains(err.Error(), "collides with the submit tool name") {
		t.Fatalf("NewRouted: got = %v, want a suspend/submit collision", err)
	}
}
