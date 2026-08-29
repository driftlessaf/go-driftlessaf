/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/executor/claudeexecutor"
	"chainguard.dev/driftlessaf/agents/executor/googleexecutor"
	"chainguard.dev/driftlessaf/agents/metaagent"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"
)

func TestNewRoutedRejectsUnsupportedRouteBeforeAdapter(t *testing.T) {
	t.Parallel()

	selection := modelrouter.Selection{Provider: "openai-provider", LogicalModel: "openai/gpt"}
	openAICalls := 0
	router := mustJudgeRouter(t, []modelrouter.Route{
		judgeTestRoute(selection, modelrouter.ProtocolOpenAIChatCompletions, "openai-deployment"),
	}, metaagent.AdapterRegistries{
		OpenAIChatCompletions: mustOpenAIJudgeAdapters(t, metaagent.OpenAIChatCompletionsRegistration{
			Provider: selection.Provider,
			Adapter: func(context.Context, modelrouter.Plan) (metaagent.OpenAIChatCompletionsBinding, error) {
				openAICalls++
				return metaagent.OpenAIChatCompletionsBinding{}, errors.New("must not be called")
			},
		}),
	})

	_, err := NewRouted(t.Context(), router, selection)
	if err == nil || !strings.Contains(err.Error(), `protocol "openai-chat-completions" is not supported`) {
		t.Fatalf("NewRouted error = %v, want unsupported protocol", err)
	}
	if openAICalls != 0 {
		t.Fatalf("OpenAI adapter calls = %d, want 0", openAICalls)
	}
}

func TestNewRoutedRejectsMissingRequirementsBeforeAdapter(t *testing.T) {
	t.Parallel()

	selection := modelrouter.Selection{Provider: "anthropic-provider", LogicalModel: "claude-sonnet-4-6"}
	route := judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "anthropic-deployment")
	route.Capabilities.MaximumOutputTokens = false
	adapterCalls := 0
	router := mustJudgeRouter(t, []modelrouter.Route{route}, metaagent.AdapterRegistries{
		AnthropicMessages: mustAnthropicJudgeAdapters(t, metaagent.AnthropicMessagesRegistration{
			Provider: selection.Provider,
			Adapter: func(context.Context, modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
				adapterCalls++
				return metaagent.AnthropicMessagesBinding{}, errors.New("must not be called")
			},
		}),
	})

	_, err := NewRouted(t.Context(), router, selection)
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("NewRouted error = %v, want ErrUnsupportedCapability", err)
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapterCalls)
	}
}

func TestNewRoutedDoesNotFallBackAfterSelectedAdapterFails(t *testing.T) {
	t.Parallel()

	const (
		selectedProvider modelrouter.Provider = "selected-provider"
		fallbackProvider modelrouter.Provider = "fallback-provider"
	)
	selection := modelrouter.Selection{Provider: selectedProvider, LogicalModel: "claude-sonnet-4-6"}
	wantErr := errors.New("selected provider authentication failed")
	selectedCalls, fallbackCalls := 0, 0
	router := mustJudgeRouter(t, []modelrouter.Route{
		judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, "selected-deployment"),
	}, metaagent.AdapterRegistries{
		AnthropicMessages: mustAnthropicJudgeAdapters(t,
			metaagent.AnthropicMessagesRegistration{
				Provider: selectedProvider,
				Adapter: func(context.Context, modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
					selectedCalls++
					return metaagent.AnthropicMessagesBinding{}, wantErr
				},
			},
			metaagent.AnthropicMessagesRegistration{
				Provider: fallbackProvider,
				Adapter: func(context.Context, modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
					fallbackCalls++
					return metaagent.AnthropicMessagesBinding{}, nil
				},
			},
		),
	})

	_, err := NewRouted(t.Context(), router, selection)
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewRouted error = %v, want selected adapter error", err)
	}
	if selectedCalls != 1 || fallbackCalls != 0 {
		t.Fatalf("adapter calls = selected:%d fallback:%d, want 1,0", selectedCalls, fallbackCalls)
	}
}

func TestNewRoutedReportsNilRouterAndUnknownSelection(t *testing.T) {
	t.Parallel()

	selection := modelrouter.Selection{Provider: "anthropic-provider", LogicalModel: "claude-sonnet-4-6"}
	if _, err := NewRouted(t.Context(), nil, selection); !errors.Is(err, metaagent.ErrInvalidRouter) {
		t.Fatalf("NewRouted with nil router error = %v, want ErrInvalidRouter", err)
	}
	router := mustJudgeRouter(t, nil, metaagent.AdapterRegistries{})
	if _, err := NewRouted(t.Context(), router, selection); !errors.Is(err, modelrouter.ErrRouteNotFound) {
		t.Fatalf("NewRouted with unknown selection error = %v, want ErrRouteNotFound", err)
	}
}

func TestRoutedClaudePreservesModesAndRouteIdentity(t *testing.T) {
	const (
		providerModelID = "anthropic-deployment-42"
		logicalModelID  = "claude-sonnet-4-6"
	)
	selection := modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: logicalModelID}
	wantModes := []JudgmentMode{GoldenMode, BenchmarkMode, StandaloneMode}
	wantPromptMarkers := []string{
		"evaluating a response against a reference answer",
		"evaluating two responses to determine which one better",
		"evaluating a response to determine how well it meets",
	}
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if requestNumber >= len(wantModes) {
			t.Errorf("unexpected request %d", requestNumber+1)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		wantMode := wantModes[requestNumber]
		wantMarker := wantPromptMarkers[requestNumber]
		requestNumber++

		var payload struct {
			Model       string   `json:"model"`
			MaxTokens   int64    `json:"max_tokens"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		if payload.Model != providerModelID {
			t.Errorf("wire model = %q, want %q", payload.Model, providerModelID)
		}
		if payload.MaxTokens != 8192 {
			t.Errorf("max_tokens = %d, want 8192", payload.MaxTokens)
		}
		if payload.Temperature == nil || *payload.Temperature != 0.1 {
			t.Errorf("temperature = %v, want 0.1", payload.Temperature)
		}
		if !strings.Contains(string(body), wantMarker) || !strings.Contains(string(body), string(wantMode)) {
			t.Errorf("request for mode %q does not contain its judge prompt: %s", wantMode, body)
		}
		writeAnthropicJudgement(w, providerModelID, wantMode)
	}))
	t.Cleanup(server.Close)

	messages := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("explicit-route-token"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	).Messages
	router := mustJudgeRouter(t, []modelrouter.Route{
		judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, providerModelID),
	}, metaagent.AdapterRegistries{
		AnthropicMessages: mustAnthropicJudgeAdapters(t, metaagent.AnthropicMessagesRegistration{
			Provider: selection.Provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
				return metaagent.NewAnthropicMessagesBinding(plan, messages, map[string]string{"workload": "judge"})
			},
		}),
	})
	judgeInstance, err := NewRouted(t.Context(), router, selection)
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}

	var traces []*agenttrace.Trace[*Judgement]
	ctx := agenttrace.WithTracer(t.Context(), agenttrace.ByCode(func(trace *agenttrace.Trace[*Judgement]) {
		traces = append(traces, trace)
	}))
	for _, request := range judgeModeRequests() {
		judgement, err := judgeInstance.Judge(ctx, request)
		if err != nil {
			t.Fatalf("Judge(%q): %v", request.Mode, err)
		}
		if judgement.Mode != request.Mode {
			t.Errorf("Judge(%q) response mode = %q", request.Mode, judgement.Mode)
		}
	}
	if requestNumber != len(wantModes) {
		t.Fatalf("provider requests = %d, want %d", requestNumber, len(wantModes))
	}
	assertJudgeRouteTraces(t, traces, providerModelID, agenttrace.SystemAnthropic, agenttrace.SystemAnthropic, logicalModelID, modelrouter.ProtocolAnthropicMessages)
}

func TestRoutedClaudeOmitsTemperatureWhenRouteDisallowsSampling(t *testing.T) {
	const providerModelID = "adaptive-deployment-84"
	selection := modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "claude-sonnet-5"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		if _, ok := payload["temperature"]; ok {
			t.Errorf("adaptive route request contains temperature: %s", body)
		}
		if strings.Contains(string(body), `"cache_control"`) {
			t.Errorf("route without prompt caching emitted a cache marker: %s", body)
		}
		writeAnthropicJudgement(w, providerModelID, StandaloneMode)
	}))
	t.Cleanup(server.Close)
	messages := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("explicit-route-token"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	).Messages
	route := judgeTestRoute(selection, modelrouter.ProtocolAnthropicMessages, providerModelID)
	route.Capabilities.PromptCaching = false
	router := mustJudgeRouter(t, []modelrouter.Route{
		route,
	}, metaagent.AdapterRegistries{
		AnthropicMessages: mustAnthropicJudgeAdapters(t, metaagent.AnthropicMessagesRegistration{
			Provider: selection.Provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
				return metaagent.NewAnthropicMessagesBinding(plan, messages, nil)
			},
		}),
	})
	judgeInstance, err := NewRouted(t.Context(), router, selection)
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}
	if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
		t.Fatalf("Judge: %v", err)
	}
}

func TestRoutedGooglePreservesStructuredOutputAndRouteIdentity(t *testing.T) {
	const (
		providerModelID = "vertex-deployment-21"
		logicalModelID  = "gemini-2.5-flash"
	)
	selection := modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: logicalModelID}
	wantModes := []JudgmentMode{GoldenMode, BenchmarkMode, StandaloneMode}
	wantPromptMarkers := []string{
		"evaluating a response against a reference answer",
		"evaluating two responses to determine which one better",
		"evaluating a response to determine how well it meets",
	}
	requestNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if requestNumber >= len(wantModes) {
			t.Errorf("unexpected request %d", requestNumber+1)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		wantMode := wantModes[requestNumber]
		wantMarker := wantPromptMarkers[requestNumber]
		requestNumber++
		if !strings.Contains(request.URL.Path, providerModelID+":generateContent") {
			t.Errorf("request path = %q, want exact provider model ID", request.URL.Path)
		}
		var payload struct {
			GenerationConfig struct {
				MaxOutputTokens int64          `json:"maxOutputTokens"`
				Temperature     *float64       `json:"temperature"`
				ResponseMIME    string         `json:"responseMimeType"`
				ResponseSchema  map[string]any `json:"responseSchema"`
			} `json:"generationConfig"`
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		config := payload.GenerationConfig
		if config.MaxOutputTokens != 8192 {
			t.Errorf("maxOutputTokens = %d, want 8192", config.MaxOutputTokens)
		}
		if config.Temperature == nil || *config.Temperature != 0.1 {
			t.Errorf("temperature = %v, want 0.1", config.Temperature)
		}
		if config.ResponseMIME != "application/json" {
			t.Errorf("responseMimeType = %q, want application/json", config.ResponseMIME)
		}
		assertJudgeResponseSchema(t, config.ResponseSchema)
		if got := payload.Labels["workload"]; got != "judge" {
			t.Errorf("workload label = %q, want judge", got)
		}
		if !strings.Contains(string(body), wantMarker) || !strings.Contains(string(body), string(wantMode)) {
			t.Errorf("request for mode %q does not contain its judge prompt: %s", wantMode, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`, judgementJSON(wantMode))
	}))
	t.Cleanup(server.Close)
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:    genai.BackendVertexAI,
		Project:    "routing-test-project",
		Location:   "us-central1",
		HTTPClient: server.Client(),
		HTTPOptions: genai.HTTPOptions{
			BaseURL: server.URL,
		},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	router := mustJudgeRouter(t, []modelrouter.Route{
		judgeTestRoute(selection, modelrouter.ProtocolGoogleGenAI, providerModelID),
	}, metaagent.AdapterRegistries{
		GoogleGenAI: mustGoogleJudgeAdapters(t, metaagent.GoogleGenAIRegistration{
			Provider: selection.Provider,
			Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.GoogleGenAIBinding, error) {
				return metaagent.NewGoogleGenAIBinding(plan, client, map[string]string{"workload": "judge"})
			},
		}),
	})
	judgeInstance, err := NewRouted(t.Context(), router, selection)
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}
	var traces []*agenttrace.Trace[*Judgement]
	ctx := agenttrace.WithTracer(t.Context(), agenttrace.ByCode(func(trace *agenttrace.Trace[*Judgement]) {
		traces = append(traces, trace)
	}))
	for _, request := range judgeModeRequests() {
		judgement, err := judgeInstance.Judge(ctx, request)
		if err != nil {
			t.Fatalf("Judge(%q): %v", request.Mode, err)
		}
		if judgement.Mode != request.Mode {
			t.Errorf("Judge(%q) response mode = %q", request.Mode, judgement.Mode)
		}
	}
	if requestNumber != len(wantModes) {
		t.Fatalf("provider requests = %d, want %d", requestNumber, len(wantModes))
	}
	assertJudgeRouteTraces(t, traces, providerModelID, "gcp.vertex_ai", agenttrace.SystemGoogleVertex, logicalModelID, modelrouter.ProtocolGoogleGenAI)
}

func TestRoutedReviewerAndJudgeSelectIndependentClaudeProviders(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "unsupported-ambient-backend")
	t.Setenv("ANTHROPIC_PROFILE", "missing-ambient-profile")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-secret-must-not-select-a-provider")

	reviewerSelection := modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "claude-sonnet-4-6"}
	judgeSelection := modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-5"}
	reviewerCalls, judgeCalls := 0, 0
	router := mustJudgeRouter(t, []modelrouter.Route{
		judgeTestRoute(reviewerSelection, modelrouter.ProtocolAnthropicMessages, "anthropic-reviewer-deployment"),
		judgeTestRoute(judgeSelection, modelrouter.ProtocolAnthropicMessages, "bedrock-judge-deployment"),
	}, metaagent.AdapterRegistries{
		AnthropicMessages: mustAnthropicJudgeAdapters(t,
			metaagent.AnthropicMessagesRegistration{
				Provider: reviewerSelection.Provider,
				Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
					reviewerCalls++
					return metaagent.NewAnthropicMessagesBinding(plan, anthropic.NewMessageService(), map[string]string{"role": "reviewer"})
				},
			},
			metaagent.AnthropicMessagesRegistration{
				Provider: judgeSelection.Provider,
				Adapter: func(_ context.Context, plan modelrouter.Plan) (metaagent.AnthropicMessagesBinding, error) {
					judgeCalls++
					return metaagent.NewAnthropicMessagesBinding(plan, anthropic.NewMessageService(), map[string]string{"role": "judge"})
				},
			},
		),
	})
	prompt, err := promptbuilder.NewPrompt("review this change")
	if err != nil {
		t.Fatalf("promptbuilder.NewPrompt: %v", err)
	}
	reviewer, err := metaagent.NewRouted[promptbuilder.Noop](t.Context(), router, reviewerSelection, metaagent.Config[*roleResponse, toolcall.EmptyTools]{
		UserPrompt: prompt,
		Tools:      toolcall.NewEmptyToolsProvider[*roleResponse](),
	})
	if err != nil {
		t.Fatalf("metaagent.NewRouted reviewer: %v", err)
	}
	judgeInstance, err := NewRouted(t.Context(), router, judgeSelection)
	if err != nil {
		t.Fatalf("judge.NewRouted: %v", err)
	}
	if reviewer == nil || judgeInstance == nil {
		t.Fatal("routed role construction returned nil")
	}
	if reviewerCalls != 1 || judgeCalls != 1 {
		t.Fatalf("adapter calls = reviewer:%d judge:%d, want 1,1", reviewerCalls, judgeCalls)
	}
}

func TestLegacyClaudeConstructionPreservesCallerOptionPrecedence(t *testing.T) {
	const overrideModel = "claude-legacy-override"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		var payload struct {
			Model       string   `json:"model"`
			MaxTokens   int64    `json:"max_tokens"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		if payload.Model != overrideModel || payload.MaxTokens != 4096 || payload.Temperature == nil || *payload.Temperature != 0.7 {
			t.Errorf("legacy Claude request = model:%q max:%d temperature:%v", payload.Model, payload.MaxTokens, payload.Temperature)
		}
		writeAnthropicJudgement(w, overrideModel, StandaloneMode)
	}))
	t.Cleanup(server.Close)
	messages := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("explicit-legacy-token"),
		option.WithBaseURL(server.URL),
		option.WithMaxRetries(0),
	).Messages
	provider := claudeexecutor.ProviderAnthropic
	judgeInstance, err := newClaudeWithMessages(claudeJudgeConstruction{
		messages:        messages,
		providerModelID: "claude-original-model",
		logicalModelID:  "claude-original-model",
		legacyProvider:  &provider,
		samplingParams:  true,
	},
		claudeexecutor.WithModel[*Request, *Judgement](overrideModel),
		claudeexecutor.WithMaxTokens[*Request, *Judgement](4096),
		claudeexecutor.WithTemperature[*Request, *Judgement](0.7),
	)
	if err != nil {
		t.Fatalf("newClaudeWithMessages: %v", err)
	}
	if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
		t.Fatalf("Judge: %v", err)
	}
}

func TestLegacyGoogleConstructionPreservesCallerOptionPrecedence(t *testing.T) {
	const overrideModel = "gemini-legacy-override"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if !strings.Contains(request.URL.Path, overrideModel+":generateContent") {
			t.Errorf("request path = %q, want caller-overridden model", request.URL.Path)
		}
		var payload struct {
			GenerationConfig struct {
				MaxOutputTokens int64    `json:"maxOutputTokens"`
				Temperature     *float64 `json:"temperature"`
			} `json:"generationConfig"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		config := payload.GenerationConfig
		if config.MaxOutputTokens != 4096 || config.Temperature == nil || *config.Temperature != 0.7 {
			t.Errorf("legacy Google request = max:%d temperature:%v", config.MaxOutputTokens, config.Temperature)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"finishReason":"STOP"}]}`, judgementJSON(StandaloneMode))
	}))
	t.Cleanup(server.Close)
	client, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		Backend:    genai.BackendVertexAI,
		Project:    "routing-test-project",
		Location:   "us-central1",
		HTTPClient: server.Client(),
		HTTPOptions: genai.HTTPOptions{
			BaseURL: server.URL,
		},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	judgeInstance, err := newGoogleWithClient(googleJudgeConstruction{
		client:          client,
		providerModelID: "gemini-original-model",
		logicalModelID:  "gemini-original-model",
		samplingParams:  true,
	},
		googleexecutor.WithModel[*Request, *Judgement](overrideModel),
		googleexecutor.WithMaxOutputTokens[*Request, *Judgement](4096),
		googleexecutor.WithTemperature[*Request, *Judgement](0.7),
	)
	if err != nil {
		t.Fatalf("newGoogleWithClient: %v", err)
	}
	if _, err := judgeInstance.Judge(t.Context(), judgeModeRequests()[2]); err != nil {
		t.Fatalf("Judge: %v", err)
	}
}

type roleResponse struct {
	Answer string `json:"answer"`
}

func judgeModeRequests() []*Request {
	return []*Request{
		{Mode: GoldenMode, ReferenceAnswer: "reference answer", ActualAnswer: "actual answer", Criterion: "fidelity"},
		{Mode: BenchmarkMode, ReferenceAnswer: "first response", ActualAnswer: "second response", Criterion: "clarity"},
		{Mode: StandaloneMode, ActualAnswer: "standalone response", Criterion: "correctness"},
	}
}

func judgementJSON(mode JudgmentMode) string {
	return fmt.Sprintf(`{"mode":%q,"score":0.9,"reasoning":"route verified","suggestions":[]}`, mode)
}

func writeAnthropicJudgement(w http.ResponseWriter, model string, mode JudgmentMode) {
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_judge","type":"message","role":"assistant","content":[],"model":%q,"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`, model),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, judgementJSON(mode)),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		var typed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &typed)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
	}
}

func assertJudgeResponseSchema(t *testing.T, schema map[string]any) {
	t.Helper()
	if !strings.EqualFold(fmt.Sprint(schema["type"]), "object") {
		t.Errorf("response schema type = %v, want object", schema["type"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Errorf("response schema properties = %T, want object", schema["properties"])
		return
	}
	wantTypes := map[string]string{
		"mode":        "string",
		"score":       "number",
		"reasoning":   "string",
		"suggestions": "array",
	}
	if len(properties) != len(wantTypes) {
		t.Errorf("response schema properties = %v, want exactly %v", properties, wantTypes)
	}
	for name, wantType := range wantTypes {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Errorf("response schema property %q = %T, want object", name, properties[name])
			continue
		}
		if !strings.EqualFold(fmt.Sprint(property["type"]), wantType) {
			t.Errorf("response schema property %q type = %v, want %s", name, property["type"], wantType)
		}
		if name == "suggestions" {
			items, ok := property["items"].(map[string]any)
			if !ok || !strings.EqualFold(fmt.Sprint(items["type"]), "string") {
				t.Errorf("response schema suggestions items = %v, want string", property["items"])
			}
		}
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Errorf("response schema required = %T, want array", schema["required"])
		return
	}
	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		requiredSet[fmt.Sprint(name)] = struct{}{}
	}
	if len(requiredSet) != len(wantTypes) {
		t.Errorf("response schema required = %v, want all four properties", required)
	}
	for name := range wantTypes {
		if _, ok := requiredSet[name]; !ok {
			t.Errorf("response schema does not require %q", name)
		}
	}
}

func assertJudgeRouteTraces(
	t *testing.T,
	traces []*agenttrace.Trace[*Judgement],
	providerModelID, providerName, legacySystem, logicalModelID string,
	protocol modelrouter.Protocol,
) {
	t.Helper()
	if len(traces) == 0 {
		t.Fatal("no judge trace was recorded")
	}
	for i, trace := range traces {
		if len(trace.Turns) != 1 {
			t.Errorf("trace %d turns = %d, want 1", i, len(trace.Turns))
			continue
		}
		turn := trace.Turns[0]
		if turn.Model != providerModelID || turn.Provider != providerName || turn.System != legacySystem || turn.LogicalModel != logicalModelID || turn.Protocol != string(protocol) {
			t.Errorf("trace %d route = model:%q provider:%q system:%q logical:%q protocol:%q", i, turn.Model, turn.Provider, turn.System, turn.LogicalModel, turn.Protocol)
		}
	}
}

func judgeTestRoute(selection modelrouter.Selection, protocol modelrouter.Protocol, providerModelID string) modelrouter.Route {
	attribution := modelrouter.Attribution{
		ProviderName: "example." + string(selection.Provider),
		LegacySystem: "example." + string(selection.Provider),
	}
	if selection.Provider == modelrouter.ProviderVertexAI {
		attribution = modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: agenttrace.SystemGoogleVertex}
	}
	if selection.Provider == modelrouter.ProviderAnthropic {
		attribution = modelrouter.Attribution{ProviderName: agenttrace.SystemAnthropic, LegacySystem: agenttrace.SystemAnthropic}
	}
	if selection.Provider == modelrouter.ProviderAWSBedrock {
		attribution = modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock}
	}
	return modelrouter.Route{
		Selection:       selection,
		Protocol:        protocol,
		ProviderModelID: providerModelID,
		Attribution:     attribution,
		Capabilities: modelrouter.Capabilities{
			ExplicitThinkingBudget: true,
			SamplingParameters:     true,
			PromptCaching:          true,
			ToolCalling:            true,
			TerminalSubmission:     true,
			SuspendResume:          true,
			MaximumOutputTokens:    true,
			RefusalRecovery:        true,
		},
	}
}

func mustJudgeRouter(t *testing.T, routes []modelrouter.Route, adapters metaagent.AdapterRegistries) *metaagent.Router {
	t.Helper()
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		t.Fatalf("modelrouter.NewRegistry: %v", err)
	}
	router, err := metaagent.NewRouter(registry, adapters)
	if err != nil {
		t.Fatalf("metaagent.NewRouter: %v", err)
	}
	return router
}

func mustAnthropicJudgeAdapters(t *testing.T, registrations ...metaagent.AnthropicMessagesRegistration) *metaagent.AnthropicMessagesAdapterRegistry {
	t.Helper()
	registry, err := metaagent.NewAnthropicMessagesAdapterRegistry(registrations...)
	if err != nil {
		t.Fatalf("metaagent.NewAnthropicMessagesAdapterRegistry: %v", err)
	}
	return registry
}

func mustGoogleJudgeAdapters(t *testing.T, registrations ...metaagent.GoogleGenAIRegistration) *metaagent.GoogleGenAIAdapterRegistry {
	t.Helper()
	registry, err := metaagent.NewGoogleGenAIAdapterRegistry(registrations...)
	if err != nil {
		t.Fatalf("metaagent.NewGoogleGenAIAdapterRegistry: %v", err)
	}
	return registry
}

func mustOpenAIJudgeAdapters(t *testing.T, registrations ...metaagent.OpenAIChatCompletionsRegistration) *metaagent.OpenAIChatCompletionsAdapterRegistry {
	t.Helper()
	registry, err := metaagent.NewOpenAIChatCompletionsAdapterRegistry(registrations...)
	if err != nil {
		t.Fatalf("metaagent.NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	return registry
}
