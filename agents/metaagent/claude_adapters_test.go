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
	"io"
	"net/http"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/toolcall"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/google/go-cmp/cmp"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

type claudeAdapterTestResponse struct {
	Answer string `json:"answer"`
}

type claudeAdapterRecordingTracer struct {
	traces []*agenttrace.Trace[*claudeAdapterTestResponse]
}

func (r *claudeAdapterRecordingTracer) NewTrace(
	ctx context.Context,
	prompt string,
	opts ...agenttrace.StartTraceOption,
) *agenttrace.Trace[*claudeAdapterTestResponse] {
	return agenttrace.NewDefaultTracer[*claudeAdapterTestResponse](ctx).NewTrace(ctx, prompt, opts...)
}

func (r *claudeAdapterRecordingTracer) RecordTrace(trace *agenttrace.Trace[*claudeAdapterTestResponse]) {
	r.traces = append(r.traces, trace)
}

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testBedrockAWSConfig(region string) aws.Config {
	return aws.Config{
		Region: region,
		Credentials: credentials.NewStaticCredentialsProvider(
			"test-access-key", "test-secret-key", "test-session-token",
		),
	}
}

func TestAnthropicDirectAdapterUsesTypedConfigAndExactPlan(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "unsupported")
	t.Setenv(anthropicauth.EnvProfile, "missing-profile")

	selection := modelrouter.Selection{
		Provider:     modelrouter.ProviderAnthropic,
		LogicalModel: "claude-sonnet-4-6@default",
	}
	plan := mustClaudePlan(t, routedTestRoute(
		selection,
		modelrouter.ProtocolAnthropicMessages,
		"claude-sonnet-4-6",
	))
	wantConfig := anthropicauth.Config{
		FederationRuleID: "fdrl_8374650192",
		OrganizationID:   "12345678-90ab-cdef-1234-567890abcdef",
		ServiceAccountID: "svac_8374650192",
		WorkspaceID:      "wrkspc_8374650192",
		Source:           anthropicauth.SourceGoogle,
	}
	factoryCalls := 0
	adapter, err := newAnthropicDirectMessagesAdapter(wantConfig, func(_ context.Context, gotConfig anthropicauth.Config) (anthropic.MessageService, error) {
		factoryCalls++
		if diff := cmp.Diff(wantConfig, gotConfig); diff != "" {
			t.Errorf("Anthropic auth config mismatch (-want, +got):\n%s", diff)
		}
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newAnthropicDirectMessagesAdapter: %v", err)
	}
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if factoryCalls != 1 {
		t.Errorf("Messages factory calls: got = %d, want = 1", factoryCalls)
	}
	if !binding.Plan().SameResolution(plan) {
		t.Error("binding plan differs from the selected route plan")
	}
	if got := binding.Plan().ProviderModelID(); got != "claude-sonnet-4-6" {
		t.Errorf("provider model ID: got = %q, want = %q", got, "claude-sonnet-4-6")
	}
	wantAttribution := modelrouter.Attribution{
		ProviderName: agenttrace.SystemAnthropic,
		LegacySystem: agenttrace.SystemAnthropic,
	}
	if got := binding.Plan().Attribution(); got != wantAttribution {
		t.Errorf("attribution: got = %+v, want = %+v", got, wantAttribution)
	}
}

func TestBedrockAdapterUsesTypedConfigAndPinsEndpoint(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "unsupported")
	t.Setenv(anthropicauth.EnvProfile, "missing-profile")
	t.Setenv("ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "https://attacker.invalid")

	selection := modelrouter.Selection{
		Provider:     modelrouter.ProviderAWSBedrock,
		LogicalModel: "claude-sonnet-5",
	}
	route := routedTestRoute(
		selection,
		modelrouter.ProtocolAnthropicMessages,
		"us.anthropic.claude-sonnet-5-v1:0",
	)
	route.Attribution = modelrouter.Attribution{
		ProviderName: agenttrace.SystemBedrock,
		LegacySystem: agenttrace.SystemBedrock,
	}
	plan := mustClaudePlan(t, route)
	wantConfig := awsauth.Config{Region: "us-east-1", Profile: "engineering-sso"}
	factoryCalls := 0
	adapter, err := newBedrockAnthropicMessagesAdapter(wantConfig, func(_ context.Context, gotConfig awsauth.Config) (aws.Config, error) {
		if gotConfig != wantConfig {
			t.Errorf("AWS auth config: got = %+v, want = %+v", gotConfig, wantConfig)
		}
		return testBedrockAWSConfig(gotConfig.Region), nil
	}, func(_ context.Context, gotConfig aws.Config, baseURL string) (anthropic.MessageService, error) {
		factoryCalls++
		if gotConfig.Region != wantConfig.Region {
			t.Errorf("loaded AWS region: got = %q, want = %q", gotConfig.Region, wantConfig.Region)
		}
		if want := "https://bedrock-mantle.us-east-1.api.aws/anthropic"; baseURL != want {
			t.Errorf("Mantle base URL: got = %q, want = %q", baseURL, want)
		}
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newBedrockAnthropicMessagesAdapter: %v", err)
	}
	binding, err := adapter(t.Context(), plan)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if factoryCalls != 1 {
		t.Errorf("Messages factory calls: got = %d, want = 1", factoryCalls)
	}
	if got := binding.Plan().ProviderModelID(); got != "us.anthropic.claude-sonnet-5-v1:0" {
		t.Errorf("provider model ID: got = %q, want = %q", got, "us.anthropic.claude-sonnet-5-v1:0")
	}
	wantAttribution := modelrouter.Attribution{
		ProviderName: agenttrace.SystemBedrock,
		LegacySystem: agenttrace.SystemBedrock,
	}
	if got := binding.Plan().Attribution(); got != wantAttribution {
		t.Errorf("attribution: got = %+v, want = %+v", got, wantAttribution)
	}
}

func TestClaudeProviderAdaptersValidatePlansBeforeClientConstruction(t *testing.T) {
	directConfig := anthropicauth.Config{
		FederationRuleID: "fdrl_1029384756",
		OrganizationID:   "abcdef12-3456-7890-abcd-ef1234567890",
		Source:           anthropicauth.SourceGoogle,
	}
	directCalls := 0
	directAdapter, err := newAnthropicDirectMessagesAdapter(directConfig, func(context.Context, anthropicauth.Config) (anthropic.MessageService, error) {
		directCalls++
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newAnthropicDirectMessagesAdapter: %v", err)
	}

	bedrockCalls := 0
	bedrockAdapter, err := newBedrockAnthropicMessagesAdapter(awsauth.Config{Region: "us-west-2"}, func(context.Context, awsauth.Config) (aws.Config, error) {
		bedrockCalls++
		return testBedrockAWSConfig("us-west-2"), nil
	}, func(context.Context, aws.Config, string) (anthropic.MessageService, error) {
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newBedrockAnthropicMessagesAdapter: %v", err)
	}

	tests := []struct {
		name      string
		adapter   AnthropicMessagesAdapter
		route     modelrouter.Route
		wantError string
	}{
		{
			name:    "Anthropic adapter receives Google protocol",
			adapter: directAdapter,
			route: routedTestRoute(
				modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "gemini-3-flash-preview"},
				modelrouter.ProtocolGoogleGenAI,
				"opaque-google-model",
			),
			wantError: "plan protocol",
		},
		{
			name:    "Anthropic adapter receives Bedrock provider",
			adapter: directAdapter,
			route: func() modelrouter.Route {
				route := routedTestRoute(
					modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-5"},
					modelrouter.ProtocolAnthropicMessages,
					"regional-profile-id",
				)
				route.Attribution = modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock}
				return route
			}(),
			wantError: "received provider",
		},
		{
			name:    "Anthropic route has Vertex attribution",
			adapter: directAdapter,
			route: func() modelrouter.Route {
				route := routedTestRoute(
					modelrouter.Selection{Provider: modelrouter.ProviderAnthropic, LogicalModel: "claude-sonnet-5"},
					modelrouter.ProtocolAnthropicMessages,
					"claude-sonnet-5",
				)
				route.Attribution = modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: agenttrace.SystemGoogleVertex}
				return route
			}(),
			wantError: "route attribution",
		},
		{
			name:    "Bedrock route has provider-shaped attribution",
			adapter: bedrockAdapter,
			route: routedTestRoute(
				modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-5"},
				modelrouter.ProtocolAnthropicMessages,
				"anthropic.claude-sonnet-5",
			),
			wantError: "route attribution",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.adapter(t.Context(), mustClaudePlan(t, tc.route))
			if !errors.Is(err, ErrInvalidBinding) {
				t.Fatalf("adapter error: got = %v, want ErrInvalidBinding", err)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("adapter error: got = %q, want substring %q", err, tc.wantError)
			}
		})
	}
	if directCalls != 0 || bedrockCalls != 0 {
		t.Errorf("Messages factory calls: got direct=%d Bedrock=%d, want 0 and 0", directCalls, bedrockCalls)
	}
}

func TestAnthropicDirectAuthenticationFailureDoesNotFallBack(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "bedrock")
	t.Setenv(anthropicauth.EnvProfile, "missing-profile")

	directSelection := modelrouter.Selection{
		Provider:     modelrouter.ProviderAnthropic,
		LogicalModel: "claude-sonnet-5",
	}
	directRoute := routedTestRoute(
		directSelection,
		modelrouter.ProtocolAnthropicMessages,
		"claude-sonnet-5",
	)
	bedrockRoute := routedTestRoute(
		modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-5"},
		modelrouter.ProtocolAnthropicMessages,
		"anthropic.claude-sonnet-5",
	)
	bedrockRoute.Attribution = modelrouter.Attribution{
		ProviderName: agenttrace.SystemBedrock,
		LegacySystem: agenttrace.SystemBedrock,
	}
	routes := mustRouteRegistry(t, directRoute, bedrockRoute)

	directAdapter, err := NewAnthropicDirectMessagesAdapter(anthropicauth.Config{
		FederationRuleID: "fdrl_5647382910",
		OrganizationID:   "fedcba98-7654-3210-fedc-ba9876543210",
		Source:           "unsupported",
	})
	if err != nil {
		t.Fatalf("NewAnthropicDirectMessagesAdapter: %v", err)
	}
	bedrockCalls := 0
	bedrockAdapter, err := newBedrockAnthropicMessagesAdapter(awsauth.Config{Region: "us-east-2"}, func(context.Context, awsauth.Config) (aws.Config, error) {
		bedrockCalls++
		return testBedrockAWSConfig("us-east-2"), nil
	}, func(context.Context, aws.Config, string) (anthropic.MessageService, error) {
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newBedrockAnthropicMessagesAdapter: %v", err)
	}
	adapters, err := NewAnthropicMessagesAdapterRegistry(
		AnthropicMessagesRegistration{Provider: modelrouter.ProviderAnthropic, Adapter: directAdapter},
		AnthropicMessagesRegistration{Provider: modelrouter.ProviderAWSBedrock, Adapter: bedrockAdapter},
	)
	if err != nil {
		t.Fatalf("NewAnthropicMessagesAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{AnthropicMessages: adapters})

	_, err = NewRouted[*testRequest](t.Context(), router, directSelection, routedClaudeTestConfig(t))
	if err == nil {
		t.Fatal("NewRouted: got nil error, want direct authentication error")
	}
	if !strings.Contains(err.Error(), `unknown identity token source "unsupported"`) {
		t.Errorf("NewRouted error: got = %q, want invalid source detail", err)
	}
	if bedrockCalls != 0 {
		t.Errorf("Bedrock adapter calls: got = %d, want = 0", bedrockCalls)
	}
}

func TestAnthropicInferenceAuthenticationFailureDoesNotFallBack(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "bedrock")
	t.Setenv(anthropicauth.EnvProfile, "missing-profile")

	directSelection := modelrouter.Selection{
		Provider:     modelrouter.ProviderAnthropic,
		LogicalModel: "claude-sonnet-5",
	}
	const providerModelID = "catalog-owned-anthropic-model-id"
	directRoute := routedTestRoute(
		directSelection,
		modelrouter.ProtocolAnthropicMessages,
		providerModelID,
	)
	bedrockRoute := routedTestRoute(
		modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-5"},
		modelrouter.ProtocolAnthropicMessages,
		"regional-bedrock-profile-id",
	)
	bedrockRoute.Attribution = modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock}
	routes := mustRouteRegistry(t, directRoute, bedrockRoute)

	httpCalls := 0
	directFactoryCalls := 0
	directAdapter, err := newAnthropicDirectMessagesAdapter(anthropicauth.Config{
		FederationRuleID: "fdrl_9081726354",
		OrganizationID:   "01234567-89ab-cdef-0123-456789abcdef",
		Source:           anthropicauth.SourceGoogle,
	}, func(context.Context, anthropicauth.Config) (anthropic.MessageService, error) {
		directFactoryCalls++
		client := anthropic.NewClient(
			option.WithoutEnvironmentDefaults(),
			option.WithAPIKey("placeholder"),
			option.WithBaseURL("https://anthropic.invalid"),
			option.WithMaxRetries(0),
			option.WithHTTPClient(&http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				httpCalls++
				body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil {
					t.Errorf("reading request body: %v", err)
				}
				var payload struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("decoding request body: %v", err)
				}
				if payload.Model != providerModelID {
					t.Errorf("request model: got = %q, want = %q", payload.Model, providerModelID)
				}
				responseBody := `{"type":"error","error":{"type":"authentication_error","message":"denied"}}`
				return &http.Response{
					StatusCode:    http.StatusUnauthorized,
					Status:        "401 Unauthorized",
					Header:        http.Header{"Content-Type": []string{"application/json"}},
					Body:          io.NopCloser(strings.NewReader(responseBody)),
					ContentLength: int64(len(responseBody)),
					Request:       request,
				}, nil
			})}),
		)
		return client.Messages, nil
	})
	if err != nil {
		t.Fatalf("newAnthropicDirectMessagesAdapter: %v", err)
	}
	bedrockCalls := 0
	bedrockAdapter, err := newBedrockAnthropicMessagesAdapter(awsauth.Config{Region: "us-east-1"}, func(context.Context, awsauth.Config) (aws.Config, error) {
		bedrockCalls++
		return testBedrockAWSConfig("us-east-1"), nil
	}, func(context.Context, aws.Config, string) (anthropic.MessageService, error) {
		return anthropic.NewMessageService(), nil
	})
	if err != nil {
		t.Fatalf("newBedrockAnthropicMessagesAdapter: %v", err)
	}
	adapters, err := NewAnthropicMessagesAdapterRegistry(
		AnthropicMessagesRegistration{Provider: modelrouter.ProviderAnthropic, Adapter: directAdapter},
		AnthropicMessagesRegistration{Provider: modelrouter.ProviderAWSBedrock, Adapter: bedrockAdapter},
	)
	if err != nil {
		t.Fatalf("NewAnthropicMessagesAdapterRegistry: %v", err)
	}
	router := mustRouter(t, routes, AdapterRegistries{AnthropicMessages: adapters})
	agent, err := NewRouted[*testRequest](t.Context(), router, directSelection, routedClaudeTestConfig(t))
	if err != nil {
		t.Fatalf("NewRouted: %v", err)
	}

	tracer := &claudeAdapterRecordingTracer{}
	ctx := agenttrace.WithTracer[*claudeAdapterTestResponse](t.Context(), tracer)
	if _, err := agent.Execute(ctx, &testRequest{}, toolcall.EmptyTools{}); err == nil {
		t.Fatal("Execute: got nil error, want authentication error")
	}
	if directFactoryCalls != 1 || httpCalls != 1 || bedrockCalls != 0 {
		t.Errorf("calls: got direct factory=%d HTTP=%d Bedrock=%d, want 1, 1, 0", directFactoryCalls, httpCalls, bedrockCalls)
	}
	if len(tracer.traces) != 1 || len(tracer.traces[0].Turns) != 1 {
		t.Fatalf("recorded traces/turns: got = %d/%d, want = 1/1", len(tracer.traces), len(tracer.traces[0].Turns))
	}
	turn := tracer.traces[0].Turns[0]
	if turn.Model != providerModelID {
		t.Errorf("trace model: got = %q, want = %q", turn.Model, providerModelID)
	}
	if turn.Provider != agenttrace.SystemAnthropic || turn.System != agenttrace.SystemAnthropic {
		t.Errorf("trace provider/system: got = %q/%q, want = %q/%q", turn.Provider, turn.System, agenttrace.SystemAnthropic, agenttrace.SystemAnthropic)
	}
	if turn.LogicalModel != directSelection.LogicalModel || turn.Protocol != string(modelrouter.ProtocolAnthropicMessages) {
		t.Errorf("trace logical model/protocol: got = %q/%q, want = %q/%q",
			turn.LogicalModel, turn.Protocol, directSelection.LogicalModel, modelrouter.ProtocolAnthropicMessages)
	}
}

func TestBedrockMantleMessagesPinsAuthEndpointAndExactModel(t *testing.T) {
	const (
		providerModelID = "us.anthropic.claude-sonnet-5-v1:0"
		baseURL         = "https://bedrock-mantle.us-east-1.api.aws/anthropic"
		ambientSecret   = "must-not-control-explicit-bedrock"
	)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", ambientSecret)
	t.Setenv("ANTHROPIC_AWS_API_KEY", ambientSecret)
	t.Setenv("ANTHROPIC_API_KEY", ambientSecret)
	t.Setenv("ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "https://attacker.invalid")

	httpCalls := 0
	messages, err := newBedrockMantleMessagesWithOptions(
		t.Context(),
		testBedrockAWSConfig("us-east-1"),
		baseURL,
		option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			httpCalls++
			if got, want := request.URL.String(), baseURL+"/v1/messages"; got != want {
				t.Errorf("request URL: got = %q, want = %q", got, want)
			}
			authorization := request.Header.Get("Authorization")
			if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 ") {
				t.Errorf("Authorization: got = %q, want SigV4", authorization)
			}
			if strings.Contains(authorization, ambientSecret) {
				t.Error("Authorization used an ambient Bedrock API key")
			}
			if got := request.Header.Get("x-api-key"); got != "" {
				t.Errorf("x-api-key: got = %q, want empty", got)
			}
			var payload struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decoding request body: %v", err)
			}
			if payload.Model != providerModelID {
				t.Errorf("request model: got = %q, want = %q", payload.Model, providerModelID)
			}
			if !payload.Stream {
				t.Error("request stream: got false, want true")
			}

			events := []string{
				`{"type":"message_start","message":{"id":"msg_bedrock","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bedrock-ready"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
				`{"type":"message_stop"}`,
			}
			var responseBody strings.Builder
			for _, event := range events {
				var typed struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal([]byte(event), &typed); err != nil {
					t.Errorf("decoding test event: %v", err)
				}
				fmt.Fprintf(&responseBody, "event: %s\ndata: %s\n\n", typed.Type, event)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(responseBody.String())),
				Request:    request,
			}, nil
		})}),
	)
	if err != nil {
		t.Fatalf("newBedrockMantleMessagesWithOptions: %v", err)
	}
	if !option.HasWithoutEnvironmentDefaults(messages.Options) {
		t.Error("Bedrock Messages options use Anthropic environment defaults")
	}

	stream := messages.NewStreaming(t.Context(), anthropic.MessageNewParams{
		Model:     providerModelID,
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
		},
	})
	var message anthropic.Message
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			t.Fatalf("accumulating Mantle event: %v", err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("streaming Mantle response: %v", err)
	}
	if httpCalls != 1 {
		t.Errorf("HTTP calls: got = %d, want = 1", httpCalls)
	}
	if len(message.Content) != 1 || message.Content[0].Text != "bedrock-ready" {
		t.Errorf("streamed content: got = %#v, want one bedrock-ready text block", message.Content)
	}
}

func TestClaudeProviderAdapterConfiguration(t *testing.T) {
	if _, err := NewAnthropicDirectMessagesAdapter(anthropicauth.Config{}); !errors.Is(err, ErrInvalidAdapter) {
		t.Errorf("NewAnthropicDirectMessagesAdapter zero config error: got = %v, want ErrInvalidAdapter", err)
	}

	tests := []struct {
		name   string
		config awsauth.Config
	}{
		{name: "empty region", config: awsauth.Config{}},
		{name: "region whitespace", config: awsauth.Config{Region: " us-east-1"}},
		{name: "region path injection", config: awsauth.Config{Region: "us-east-1/other"}},
		{name: "region suffix injection", config: awsauth.Config{Region: "us-east-1.example.com"}},
		{name: "profile whitespace", config: awsauth.Config{Region: "us-east-1", Profile: " profile"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewBedrockAnthropicMessagesAdapter(tc.config); !errors.Is(err, ErrInvalidAdapter) {
				t.Errorf("NewBedrockAnthropicMessagesAdapter error: got = %v, want ErrInvalidAdapter", err)
			}
		})
	}
}

func TestNewKeepsLegacyClaudeBackendEnvironmentSelection(t *testing.T) {
	t.Setenv("CLAUDE_BACKEND", "unsupported")
	t.Setenv(anthropicauth.EnvProfile, "")

	_, err := New[*testRequest](
		t.Context(),
		"unused-project",
		"unused-region",
		"claude-sonnet-5",
		routedClaudeTestConfig(t),
	)
	if err == nil {
		t.Fatal("New: got nil error, want legacy CLAUDE_BACKEND validation error")
	}
	if !strings.Contains(err.Error(), "unknown CLAUDE_BACKEND value") {
		t.Errorf("New error: got = %q, want legacy CLAUDE_BACKEND detail", err)
	}
}

func mustClaudePlan(t *testing.T, route modelrouter.Route) modelrouter.Plan {
	t.Helper()
	registry := mustRouteRegistry(t, route)
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return plan
}

func routedClaudeTestConfig(t *testing.T) Config[*claudeAdapterTestResponse, toolcall.EmptyTools] {
	t.Helper()
	prompt, err := promptbuilder.NewPrompt("answer")
	if err != nil {
		t.Fatalf("NewPrompt: %v", err)
	}
	return Config[*claudeAdapterTestResponse, toolcall.EmptyTools]{
		UserPrompt: prompt,
		Tools:      toolcall.NewEmptyToolsProvider[*claudeAdapterTestResponse](),
	}
}
