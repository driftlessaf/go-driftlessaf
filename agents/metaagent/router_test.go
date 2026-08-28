/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"golang.org/x/oauth2"
	"google.golang.org/genai"
)

func TestNewRoutedRejectsUnsupportedRequirementsBeforeAdapter(t *testing.T) {
	t.Parallel()

	selection := modelrouter.Selection{Provider: "test-provider", LogicalModel: "test/model"}
	route := routedTestRoute(selection, modelrouter.ProtocolOpenAIChatCompletions, "deployment-42")
	route.Capabilities.SuspendResume = false
	routes := mustRouteRegistry(t, route)

	adapterCalls := 0
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{
		Provider: selection.Provider,
		Adapter: func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
			adapterCalls++
			return OpenAIChatCompletionsBinding{}, errors.New("must not be called")
		},
	})
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{OpenAIChatCompletions: adapters})
	config := routedTestConfig(t)
	config.SuspendToolName = "ask_friend"

	_, err = NewRouted[*testRequest](t.Context(), router, selection, config)
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("NewRouted error = %v, want ErrUnsupportedCapability", err)
	}
	if adapterCalls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapterCalls)
	}
}

func TestNewRoutedMaximumOutputTokensAreRequestedOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	selection := modelrouter.Selection{Provider: "test-provider", LogicalModel: "test/model"}
	route := routedTestRoute(selection, modelrouter.ProtocolOpenAIChatCompletions, "deployment-42")
	route.Capabilities.MaximumOutputTokens = false
	routes := mustRouteRegistry(t, route)
	adapterCalls := 0
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{
		Provider: selection.Provider,
		Adapter: func(_ context.Context, plan modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
			adapterCalls++
			return NewOpenAIChatCompletionsBinding(plan, openai.Client{}, "max_tokens", nil)
		},
	})
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{OpenAIChatCompletions: adapters})

	if _, err := NewRouted[*testRequest](t.Context(), router, selection, routedTestConfig(t)); err != nil {
		t.Fatalf("NewRouted with protocol default: %v", err)
	}
	if adapterCalls != 1 {
		t.Fatalf("adapter calls after default construction = %d, want 1", adapterCalls)
	}

	config := routedTestConfig(t)
	config.MaxTokens = 4096
	_, err = NewRouted[*testRequest](t.Context(), router, selection, config)
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("NewRouted with explicit max tokens error = %v, want ErrUnsupportedCapability", err)
	}
	if adapterCalls != 1 {
		t.Fatalf("adapter calls after unsupported max tokens = %d, want unchanged 1", adapterCalls)
	}
}

func TestRequirementsForConfig(t *testing.T) {
	t.Parallel()

	base := modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true}
	tests := []struct {
		name     string
		protocol modelrouter.Protocol
		mutate   func(*Config[*basetenTestResponse, toolcall.EmptyTools])
		want     modelrouter.Requirements
	}{
		{name: "defaults do not request optional capabilities", protocol: modelrouter.ProtocolGoogleGenAI, want: base},
		{
			name:     "effort",
			protocol: modelrouter.ProtocolGoogleGenAI,
			mutate:   func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) { config.Effort = effort.High },
			want:     modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true, Effort: effort.High},
		},
		{
			name:     "thinking budget",
			protocol: modelrouter.ProtocolGoogleGenAI,
			mutate:   func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) { config.ThinkingBudget = 2048 },
			want:     modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true, ExplicitThinkingBudget: true},
		},
		{
			name:     "Anthropic suffix requests cache boundary",
			protocol: modelrouter.ProtocolAnthropicMessages,
			mutate: func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) {
				config.UserPromptSuffix = config.UserPrompt
			},
			want: modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true, PromptCaching: true},
		},
		{
			name:     "Google suffix remains concatenation only",
			protocol: modelrouter.ProtocolGoogleGenAI,
			mutate: func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) {
				config.UserPromptSuffix = config.UserPrompt
			},
			want: base,
		},
		{
			name:     "OpenAI suffix remains concatenation only",
			protocol: modelrouter.ProtocolOpenAIChatCompletions,
			mutate: func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) {
				config.UserPromptSuffix = config.UserPrompt
			},
			want: base,
		},
		{
			name:     "maximum output tokens",
			protocol: modelrouter.ProtocolOpenAIChatCompletions,
			mutate:   func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) { config.MaxTokens = 4096 },
			want:     modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true, MaximumOutputTokens: true},
		},
		{
			name:     "refusal recovery",
			protocol: modelrouter.ProtocolAnthropicMessages,
			mutate: func(config *Config[*basetenTestResponse, toolcall.EmptyTools]) {
				config.RefusalNudgeMaxRetries = 2
			},
			want: modelrouter.Requirements{ToolCalling: true, TerminalSubmission: true, RefusalRecovery: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := routedTestConfig(t)
			if tc.mutate != nil {
				tc.mutate(&config)
			}
			got := requirementsForConfig(tc.protocol, config)
			if got != tc.want {
				t.Errorf("requirementsForConfig = %+v, want %+v", got, tc.want)
			}
			if got.SamplingParameters {
				t.Error("requirementsForConfig unexpectedly requested caller sampling parameters")
			}
		})
	}
}

func TestNewRoutedInvokesExactlyOneAdapterWithoutFallback(t *testing.T) {
	t.Parallel()

	const (
		selectedProvider modelrouter.Provider = "selected-provider"
		fallbackProvider modelrouter.Provider = "fallback-provider"
	)
	selection := modelrouter.Selection{Provider: selectedProvider, LogicalModel: "test/model"}
	routes := mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolOpenAIChatCompletions, "selected-deployment"))
	wantErr := errors.New("selected adapter authentication failed")
	selectedCalls, fallbackCalls, googleCalls := 0, 0, 0
	openAIAdapters, err := NewOpenAIChatCompletionsAdapterRegistry(
		OpenAIChatCompletionsRegistration{
			Provider: selectedProvider,
			Adapter: func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
				selectedCalls++
				return OpenAIChatCompletionsBinding{}, wantErr
			},
		},
		OpenAIChatCompletionsRegistration{
			Provider: fallbackProvider,
			Adapter: func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
				fallbackCalls++
				return OpenAIChatCompletionsBinding{}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	googleAdapters, err := NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{
		Provider: selectedProvider,
		Adapter: func(context.Context, modelrouter.Plan) (GoogleGenAIBinding, error) {
			googleCalls++
			return GoogleGenAIBinding{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewGoogleGenAIAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{
		GoogleGenAI:           googleAdapters,
		OpenAIChatCompletions: openAIAdapters,
	})

	_, err = NewRouted[*testRequest](t.Context(), router, selection, routedTestConfig(t))
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewRouted error = %v, want selected adapter error", err)
	}
	if selectedCalls != 1 || fallbackCalls != 0 || googleCalls != 0 {
		t.Fatalf("adapter calls = selected:%d fallback:%d google:%d, want 1,0,0", selectedCalls, fallbackCalls, googleCalls)
	}
}

func TestNewRoutedSupportsExtensionProviderWithoutRouterChanges(t *testing.T) {
	t.Parallel()

	const provider modelrouter.Provider = "test-provider"
	selection := modelrouter.Selection{Provider: provider, LogicalModel: "test/model"}
	routes := mustRouteRegistry(t, routedTestRoute(selection, modelrouter.ProtocolOpenAIChatCompletions, "opaque-deployment-id"))
	adapterCalls := 0
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{
		Provider: provider,
		Adapter: func(_ context.Context, plan modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
			adapterCalls++
			return NewOpenAIChatCompletionsBinding(plan, openai.Client{}, "max_tokens", map[string]string{"provider": "test"})
		},
	})
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{OpenAIChatCompletions: adapters})

	agent, err := NewRouted[*testRequest](t.Context(), router, selection, routedTestConfig(t))
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}
	if agent == nil {
		t.Fatal("NewRouted returned a nil agent")
	}
	if adapterCalls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapterCalls)
	}
}

func TestNewRoutedRejectsAdapterPlanSubstitution(t *testing.T) {
	t.Parallel()

	const provider modelrouter.Provider = "test-provider"
	selected := modelrouter.Selection{Provider: provider, LogicalModel: "test/model"}
	substitute := modelrouter.Selection{Provider: provider, LogicalModel: "other/model"}
	routes := mustRouteRegistry(t,
		routedTestRoute(selected, modelrouter.ProtocolOpenAIChatCompletions, "selected-deployment"),
		routedTestRoute(substitute, modelrouter.ProtocolOpenAIChatCompletions, "other-deployment"),
	)
	substitutePlan, err := routes.Resolve(substitute)
	if err != nil {
		t.Fatalf("Resolve substitute: %v", err)
	}
	adapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{
		Provider: provider,
		Adapter: func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
			return NewOpenAIChatCompletionsBinding(substitutePlan, openai.Client{}, "max_tokens", nil)
		},
	})
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{OpenAIChatCompletions: adapters})

	_, err = NewRouted[*testRequest](t.Context(), router, selected, routedTestConfig(t))
	if !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("NewRouted error = %v, want ErrInvalidBinding", err)
	}
	if !strings.Contains(err.Error(), "substituted") {
		t.Errorf("NewRouted error = %q, want substitution detail", err)
	}
}

func TestVertexRoutesCoverAllControlledProtocols(t *testing.T) {
	t.Parallel()

	routes := []modelrouter.Route{
		routedTestRoute(
			modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "gemini-3-flash-preview"},
			modelrouter.ProtocolGoogleGenAI,
			"publishers/google/models/gemini-3-flash-preview",
		),
		routedTestRoute(
			modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "claude-sonnet-5"},
			modelrouter.ProtocolAnthropicMessages,
			"publishers/anthropic/models/claude-sonnet-5@20260801",
		),
		routedTestRoute(
			modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "google/gemini-3-flash-preview"},
			modelrouter.ProtocolOpenAIChatCompletions,
			"google/gemini-3-flash-preview",
		),
	}
	routeRegistry := mustRouteRegistry(t, routes...)

	googleCalls := 0
	googleAdapter, err := newVertexGoogleGenAIAdapter("test-project", "us-central1", func(_ context.Context, config *genai.ClientConfig) (*genai.Client, error) {
		googleCalls++
		if config.Project != "test-project" || config.Location != "us-central1" || config.Backend != genai.BackendVertexAI {
			t.Errorf("Google client config = %+v", config)
		}
		return &genai.Client{}, nil
	})
	if err != nil {
		t.Fatalf("newVertexGoogleGenAIAdapter: %v", err)
	}
	googleAdapters, err := NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: googleAdapter})
	if err != nil {
		t.Fatalf("NewGoogleGenAIAdapterRegistry: %v", err)
	}

	claudeCalls := 0
	claudeAdapter, err := newVertexAnthropicMessagesAdapter("test-project", "us-central1", func(_ context.Context, projectID, region string) (anthropic.MessageService, error) {
		claudeCalls++
		if projectID != "test-project" || region != "us-central1" {
			t.Errorf("Anthropic factory project/region = %q/%q", projectID, region)
		}
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newVertexAnthropicMessagesAdapter: %v", err)
	}
	claudeAdapters, err := NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: claudeAdapter})
	if err != nil {
		t.Fatalf("NewAnthropicMessagesAdapterRegistry: %v", err)
	}

	openAICalls := 0
	openAIAdapter, err := newVertexOpenAIChatCompletionsAdapter("test-project", "global", func(_ context.Context, scopes ...string) (oauth2.TokenSource, error) {
		openAICalls++
		if len(scopes) != 1 || scopes[0] != vertexCloudPlatformScope {
			t.Errorf("token scopes = %v", scopes)
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), nil
	})
	if err != nil {
		t.Fatalf("newVertexOpenAIChatCompletionsAdapter: %v", err)
	}
	openAIAdapters, err := NewOpenAIChatCompletionsAdapterRegistry(OpenAIChatCompletionsRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: openAIAdapter})
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsAdapterRegistry: %v", err)
	}

	router := mustRouter(t, routeRegistry, AdapterRegistries{
		GoogleGenAI:           googleAdapters,
		AnthropicMessages:     claudeAdapters,
		OpenAIChatCompletions: openAIAdapters,
	})
	for _, route := range routes {
		agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, routedTestConfig(t))
		if err != nil {
			t.Errorf("NewRouted(%s): %v", route.Protocol, err)
			continue
		}
		if agent == nil {
			t.Errorf("NewRouted(%s) returned nil agent", route.Protocol)
		}
	}
	if googleCalls != 1 || claudeCalls != 1 || openAICalls != 1 {
		t.Errorf("Vertex adapter calls = Google:%d Claude:%d OpenAI:%d, want 1 each", googleCalls, claudeCalls, openAICalls)
	}
}

func TestValidateVertexPlanRejectsMisattributionBeforeClientConstruction(t *testing.T) {
	t.Parallel()

	protocols := []struct {
		protocol modelrouter.Protocol
		logical  string
	}{
		{modelrouter.ProtocolGoogleGenAI, "gemini-3-flash-preview"},
		{modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"},
		{modelrouter.ProtocolOpenAIChatCompletions, "google/gemini-3-flash-preview"},
	}
	for _, tc := range protocols {
		t.Run(string(tc.protocol), func(t *testing.T) {
			t.Parallel()
			route := routedTestRoute(
				modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: tc.logical},
				tc.protocol,
				"provider-model",
			)
			route.Attribution = modelrouter.Attribution{ProviderName: "anthropic", LegacySystem: "anthropic"}
			registry := mustRouteRegistry(t, route)
			plan, err := registry.Resolve(route.Selection)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			err = validateVertexPlan(plan, tc.protocol)
			if !errors.Is(err, ErrInvalidBinding) {
				t.Fatalf("validateVertexPlan error = %v, want ErrInvalidBinding", err)
			}
			if !strings.Contains(err.Error(), "gcp.vertex_ai") {
				t.Errorf("validateVertexPlan error = %q, want canonical attribution detail", err)
			}
		})
	}
}

func TestVertexAdapterConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		projectID string
		region    string
		wantURL   string
		wantErr   bool
	}{
		{
			name:      "regional endpoint",
			projectID: "test-project",
			region:    "us-central1",
			wantURL:   "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/test-project/locations/us-central1/endpoints/openapi",
		},
		{
			name:      "global endpoint",
			projectID: "test-project",
			region:    "global",
			wantURL:   "https://aiplatform.googleapis.com/v1beta1/projects/test-project/locations/global/endpoints/openapi",
		},
		{name: "empty project", region: "global", wantErr: true},
		{name: "empty region", projectID: "test-project", wantErr: true},
		{name: "project whitespace", projectID: " test-project", region: "global", wantErr: true},
		{name: "region whitespace", projectID: "test-project", region: "global ", wantErr: true},
		{name: "project path injection", projectID: "test-project/locations/other", region: "global", wantErr: true},
		{name: "project query injection", projectID: "test-project?alt=media", region: "global", wantErr: true},
		{name: "region fragment changes authority", projectID: "test-project", region: "attacker.example#", wantErr: true},
		{name: "region userinfo changes authority", projectID: "test-project", region: "us-central1@attacker.example", wantErr: true},
		{name: "region path injection", projectID: "test-project", region: "us-central1/path", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config, err := newVertexConfig(tc.projectID, tc.region)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidAdapter) {
					t.Fatalf("newVertexConfig error = %v, want ErrInvalidAdapter", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newVertexConfig: %v", err)
			}
			if got := config.openAIBaseURL(); got != tc.wantURL {
				t.Errorf("openAIBaseURL = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestRoutedAttributionComposesPlanIdentities(t *testing.T) {
	t.Parallel()

	route := routedTestRoute(
		modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "claude-sonnet-5"},
		modelrouter.ProtocolAnthropicMessages,
		"opaque-provider-model",
	)
	registry := mustRouteRegistry(t, route)
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := agenttrace.Attribution{
		ProviderName: "gcp.vertex_ai",
		System:       agenttrace.SystemGoogleVertex,
		LogicalModel: "claude-sonnet-5",
		Protocol:     string(modelrouter.ProtocolAnthropicMessages),
	}
	if got := routedAttribution(plan); got != want {
		t.Errorf("routedAttribution = %+v, want %+v", got, want)
	}
}

func TestAdapterRegistriesRejectInvalidRegistrationsDeterministically(t *testing.T) {
	t.Parallel()

	adapter := OpenAIChatCompletionsAdapter(func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
		return OpenAIChatCompletionsBinding{}, nil
	})
	_, err := NewOpenAIChatCompletionsAdapterRegistry(
		OpenAIChatCompletionsRegistration{Provider: "test-provider", Adapter: adapter},
		OpenAIChatCompletionsRegistration{Provider: "test-provider", Adapter: adapter},
	)
	if !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("duplicate registration error = %v, want ErrDuplicateAdapter", err)
	}
	want := `registration 1: duplicate model adapter for provider "test-provider" and protocol "openai-chat-completions" (first registered at registration 0)`
	if err.Error() != want {
		t.Errorf("duplicate registration error = %q, want %q", err, want)
	}

	_, err = NewGoogleGenAIAdapterRegistry(GoogleGenAIRegistration{Provider: "Bad Provider"})
	if !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("invalid provider error = %v, want ErrInvalidAdapter", err)
	}
	_, err = NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{Provider: "test-provider"})
	if !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("nil adapter error = %v, want ErrInvalidAdapter", err)
	}
}

func routedTestConfig(t *testing.T) Config[*basetenTestResponse, toolcall.EmptyTools] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("payload")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	return Config[*basetenTestResponse, toolcall.EmptyTools]{
		UserPrompt: prompt,
		Tools:      toolcall.NewEmptyToolsProvider[*basetenTestResponse](),
	}
}

func routedTestRoute(selection modelrouter.Selection, protocol modelrouter.Protocol, providerModelID string) modelrouter.Route {
	attribution := modelrouter.Attribution{ProviderName: string(selection.Provider), LegacySystem: string(selection.Provider)}
	if selection.Provider == modelrouter.ProviderVertexAI {
		attribution = modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: agenttrace.SystemGoogleVertex}
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

func mustRouteRegistry(t *testing.T, routes ...modelrouter.Route) *modelrouter.Registry {
	t.Helper()
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		t.Fatalf("modelrouter.NewRegistry: %v", err)
	}
	return registry
}

func mustRouter(t *testing.T, routes *modelrouter.Registry, adapters AdapterRegistries) *Router {
	t.Helper()
	router, err := NewRouter(routes, adapters)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}
