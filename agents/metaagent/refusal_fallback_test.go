/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	fallbackTestConfigured = "claude-sonnet-5-5"
	fallbackTestFallback   = "claude-opus-5-5"
)

// writeRefusedTurn streams a turn the safety classifier refused.
func writeRefusedTurn(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "message_start", "message": map[string]any{
			"id": "msg-refused", "type": "message", "role": "assistant", "model": fallbackTestConfigured,
			"content": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 0},
		}},
		{"type": "message_delta", "delta": map[string]any{
			"stop_reason":  "refusal",
			"stop_details": map[string]any{"type": "refusal", "category": "reasoning_extraction", "explanation": "scripted"},
		}, "usage": map[string]any{"output_tokens": 0}},
		{"type": "message_stop"},
	} {
		data, err := json.Marshal(event)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}

// fallbackTestRouter serves every declared model from one endpoint that
// refuses the configured model's turns and has the fallback submit. models
// returns the model each request named, in order.
func fallbackTestRouter(t *testing.T, routes ...modelrouter.Route) (router *Router, models func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		sent []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		data, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(data, &body)
		}
		if err != nil {
			t.Errorf("decoding a request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		sent = append(sent, body.Model)
		n := int32(len(sent))
		mu.Unlock()
		if body.Model == fallbackTestConfigured {
			writeRefusedTurn(t, w)
			return
		}
		writeToolUseTurn(t, w, n, "submit_result", `{"reasoning":"complete","result":{"answer":"done"}}`)
	}))
	t.Cleanup(srv.Close)

	adapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{
		Provider: "scripted",
		Adapter: func(_ context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
			client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("scripted"), option.WithMaxRetries(0))
			return NewAnthropicMessagesBinding(plan, client.Messages, map[string]string{"region": "scripted-region"})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mustRouter(t, mustRouteRegistry(t, routes...), AdapterRegistries{AnthropicMessages: adapters}), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sent)
	}
}

func fallbackTestRoute(model string) modelrouter.Route {
	return routedTestRoute(modelrouter.Selection{Provider: "scripted", LogicalModel: model}, modelrouter.ProtocolAnthropicMessages, model)
}

// TestRoutedRefusalFallback: a routed agent with RefusalFallbackModel sends
// the refused turn again to the fallback, bound through the same Router, and
// the trace attributes each turn to the route that served it.
func TestRoutedRefusalFallback(t *testing.T) {
	router, models := fallbackTestRouter(t, fallbackTestRoute(fallbackTestConfigured), fallbackTestRoute(fallbackTestFallback))
	cfg := routedTestConfig(t)
	cfg.RefusalFallbackModel = fallbackTestFallback
	agent, err := NewRouted[*testRequest](t.Context(), router, modelrouter.Selection{Provider: "scripted", LogicalModel: fallbackTestConfigured}, cfg)
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}
	tracer := new(basetenRecordingTracer)

	resp, err := agent.Execute(agenttrace.WithTracer[*basetenTestResponse](t.Context(), tracer), &testRequest{}, toolcall.EmptyTools{})

	if err != nil || resp == nil || resp.Answer != "done" {
		t.Fatalf("Execute: resp = %+v, err = %v, want the fallback's submission", resp, err)
	}
	if got, want := models(), []string{fallbackTestConfigured, fallbackTestFallback}; !slices.Equal(got, want) {
		t.Errorf("request models: got = %q, want = %q", got, want)
	}
	if len(tracer.traces) != 1 {
		t.Fatalf("recorded traces: got = %d, want = 1", len(tracer.traces))
	}
	turns := tracer.traces[0].Turns
	if len(turns) != 2 {
		t.Fatalf("trace turns: got = %d, want = 2", len(turns))
	}
	for i, want := range []string{fallbackTestConfigured, fallbackTestFallback} {
		if turns[i].LogicalModel != want || turns[i].Provider != "scripted" || turns[i].ServingLocation != "scripted-region" {
			t.Errorf("trace turn %d: got logical model %q, provider %q, location %q, want %q on scripted in scripted-region",
				i, turns[i].LogicalModel, turns[i].Provider, turns[i].ServingLocation, want)
		}
	}
	if len(turns[0].Errors) != 1 || !strings.Contains(turns[0].Errors[0], "reasoning_extraction") {
		t.Errorf("the refused turn's errors: got %q, want the refusal", turns[0].Errors)
	}
}

// TestRoutedRefusalFallbackIsValidatedAtConstruction: a fallback the Router
// cannot serve with every capability the Config requires, or whose route does
// not take the sampling parameters the selected route sends, fails NewRouted,
// as does one off the Anthropic Messages protocol or on the compatibility
// path.
func TestRoutedRefusalFallbackIsValidatedAtConstruction(t *testing.T) {
	noOutputCap := fallbackTestRoute(fallbackTestFallback)
	noOutputCap.Capabilities.MaximumOutputTokens = false
	// Both models take sampling parameters; the fallback's route narrows
	// them out.
	const sampling, samplingFallback = "claude-sonnet-4-6", "claude-opus-4-6"
	noSampling := fallbackTestRoute(samplingFallback)
	noSampling.Capabilities.SamplingParameters = false
	for _, tc := range []struct {
		name     string
		routes   []modelrouter.Route
		selected string // fallbackTestConfigured when empty
		fallback string
		mutate   func(*Config[*basetenTestResponse, toolcall.EmptyTools])
		wantErr  string
	}{
		{
			name:     "a fallback route without the selected route's sampling parameters",
			routes:   []modelrouter.Route{fallbackTestRoute(sampling), noSampling},
			selected: sampling,
			fallback: samplingFallback,
			wantErr:  "does not take the sampling parameters",
		},
		{name: "an undeclared fallback", routes: []modelrouter.Route{fallbackTestRoute(fallbackTestConfigured)}, fallback: fallbackTestFallback, wantErr: "refusal fallback"},
		{name: "the selected model", routes: []modelrouter.Route{fallbackTestRoute(fallbackTestConfigured)}, fallback: fallbackTestConfigured, wantErr: "is the selected model"},
		{
			name:     "a fallback without a required capability",
			routes:   []modelrouter.Route{fallbackTestRoute(fallbackTestConfigured), noOutputCap},
			fallback: fallbackTestFallback,
			mutate:   func(cfg *Config[*basetenTestResponse, toolcall.EmptyTools]) { cfg.MaxTokens = 4096 },
			wantErr:  "maximum output tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, models := fallbackTestRouter(t, tc.routes...)
			cfg := routedTestConfig(t)
			cfg.RefusalFallbackModel = tc.fallback
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			selection := modelrouter.Selection{Provider: "scripted", LogicalModel: cmp.Or(tc.selected, fallbackTestConfigured)}
			_, err := NewRouted[*testRequest](t.Context(), router, selection, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewRouted: got %v, want an error naming %q", err, tc.wantErr)
			}
			if got := models(); len(got) != 0 {
				t.Errorf("requests: got %q, want none", got)
			}
		})
	}

	cfg := routedTestConfig(t)
	cfg.RefusalFallbackModel = fallbackTestFallback
	for _, protocol := range []modelrouter.Protocol{modelrouter.ProtocolGoogleGenAI, modelrouter.ProtocolOpenAIChatCompletions, modelrouter.ProtocolOpenAIResponses} {
		if err := validateRoutedConfigForProtocol(protocol, "", cfg); err == nil || !strings.Contains(err.Error(), "RefusalFallbackModel") {
			t.Errorf("validateRoutedConfigForProtocol(%s): got %v, want RefusalFallbackModel refused", protocol, err)
		}
	}
	if _, err := New[*testRequest](t.Context(), "test-project", "global", fallbackTestConfigured, cfg); err == nil || !strings.Contains(err.Error(), "routed construction") {
		t.Errorf("New: got %v, want RefusalFallbackModel refused on the compatibility path", err)
	}
}
