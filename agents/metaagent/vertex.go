/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/genai"
)

const vertexCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

type vertexConfig struct {
	projectID string
	region    string
}

type vertexGoogleGenAIClientFactory func(context.Context, *genai.ClientConfig) (*genai.Client, error)
type vertexAnthropicMessagesFactory func(context.Context, string, string) (anthropic.MessageService, error)
type vertexTokenSourceFactory func(context.Context, ...string) (oauth2.TokenSource, error)

// NewVertexGoogleGenAIAdapter constructs a Vertex AI adapter for the Google
// Gen AI protocol. Application Default Credentials remain captured by the SDK
// client and never enter a route or plan.
func NewVertexGoogleGenAIAdapter(projectID, region string) (GoogleGenAIAdapter, error) {
	return newVertexGoogleGenAIAdapter(projectID, region, genai.NewClient)
}

func newVertexGoogleGenAIAdapter(projectID, region string, newClient vertexGoogleGenAIClientFactory) (GoogleGenAIAdapter, error) {
	return newVertexGoogleGenAIAdapterWithRequestTimeout(projectID, region, 0, newClient)
}

// NewVertexGoogleGenAIAdapterWithRequestTimeout constructs a Vertex AI adapter
// whose individual Google Gen AI requests are bounded by requestTimeout. The
// bound applies to each attempt independently, so the executor's retry policy
// can recover from a stalled request while the caller's context remains the
// authority for the overall agent deadline.
func NewVertexGoogleGenAIAdapterWithRequestTimeout(projectID, region string, requestTimeout time.Duration) (GoogleGenAIAdapter, error) {
	if requestTimeout <= 0 {
		return nil, fmt.Errorf("%w: Vertex Google Gen AI request timeout must be greater than zero", ErrInvalidAdapter)
	}
	return newVertexGoogleGenAIAdapterWithRequestTimeout(projectID, region, requestTimeout, genai.NewClient)
}

func newVertexGoogleGenAIAdapterWithRequestTimeout(projectID, region string, requestTimeout time.Duration, newClient vertexGoogleGenAIClientFactory) (GoogleGenAIAdapter, error) {
	config, err := newVertexConfig(projectID, region)
	if err != nil {
		return nil, err
	}
	if newClient == nil {
		return nil, fmt.Errorf("%w: Vertex Google Gen AI client factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (GoogleGenAIBinding, error) {
		if err := validateVertexPlan(plan, modelrouter.ProtocolGoogleGenAI); err != nil {
			return GoogleGenAIBinding{}, err
		}
		clientConfig := &genai.ClientConfig{
			Project:  config.projectID,
			Location: config.region,
			Backend:  genai.BackendVertexAI,
		}
		if requestTimeout > 0 {
			clientConfig.HTTPOptions.Timeout = &requestTimeout
		}
		client, err := newClient(ctx, clientConfig)
		if err != nil {
			return GoogleGenAIBinding{}, fmt.Errorf("creating Google AI client: %w", err)
		}
		binding, err := NewGoogleGenAIBinding(plan, client, config.resourceLabels(plan))
		if err != nil {
			return GoogleGenAIBinding{}, err
		}
		binding.retryRequestTimeouts = requestTimeout > 0
		return binding, nil
	}, nil
}

// NewVertexAnthropicMessagesAdapter constructs a Vertex AI adapter for the
// Anthropic Messages protocol. Application Default Credentials are bound only
// while constructing the typed Messages service.
func NewVertexAnthropicMessagesAdapter(projectID, region string) (AnthropicMessagesAdapter, error) {
	return newVertexAnthropicMessagesAdapter(projectID, region, func(ctx context.Context, projectID, region string) (anthropic.MessageService, error) {
		return anthropicauth.NewClient(ctx, projectID, region, anthropicauth.Config{}).Messages, nil
	})
}

func newVertexAnthropicMessagesAdapter(projectID, region string, newMessages vertexAnthropicMessagesFactory) (AnthropicMessagesAdapter, error) {
	config, err := newVertexConfig(projectID, region)
	if err != nil {
		return nil, err
	}
	if newMessages == nil {
		return nil, fmt.Errorf("%w: Vertex Anthropic Messages factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
		if err := validateVertexPlan(plan, modelrouter.ProtocolAnthropicMessages); err != nil {
			return AnthropicMessagesBinding{}, err
		}
		messages, err := newMessages(ctx, config.projectID, config.region)
		if err != nil {
			return AnthropicMessagesBinding{}, fmt.Errorf("creating Vertex Anthropic Messages client: %w", err)
		}
		return NewAnthropicMessagesBinding(plan, messages, config.resourceLabels(plan))
	}, nil
}

// NewVertexOpenAIChatCompletionsAdapter constructs a Vertex AI adapter for the
// OpenAI Chat Completions protocol. The OAuth token source remains attached to
// the SDK transport and never enters a route or plan.
func NewVertexOpenAIChatCompletionsAdapter(projectID, region string) (OpenAIChatCompletionsAdapter, error) {
	return newVertexOpenAIChatCompletionsAdapter(projectID, region, google.DefaultTokenSource)
}

func newVertexOpenAIChatCompletionsAdapter(projectID, region string, newTokenSource vertexTokenSourceFactory) (OpenAIChatCompletionsAdapter, error) {
	config, err := newVertexConfig(projectID, region)
	if err != nil {
		return nil, err
	}
	if newTokenSource == nil {
		return nil, fmt.Errorf("%w: Vertex token-source factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
		if err := validateVertexPlan(plan, modelrouter.ProtocolOpenAIChatCompletions); err != nil {
			return OpenAIChatCompletionsBinding{}, err
		}
		tokenSource, err := newTokenSource(ctx, vertexCloudPlatformScope)
		if err != nil {
			return OpenAIChatCompletionsBinding{}, fmt.Errorf("creating GCP token source: %w", err)
		}
		if tokenSource == nil {
			return OpenAIChatCompletionsBinding{}, fmt.Errorf("creating GCP token source: returned nil token source")
		}

		return NewOpenAIChatCompletionsBinding(
			plan,
			openai.NewClient(
				option.WithBaseURL(config.openAIBaseURL()),
				// The oauth2 transport replaces this non-empty placeholder with a
				// GCP access token on every request.
				option.WithAPIKey("vertex-ai-auth"),
				option.WithHTTPClient(&http.Client{
					Transport: &oauth2.Transport{Source: tokenSource},
				}),
			),
			openaiexecutor.TokenLimitMaxCompletionTokens,
			config.resourceLabels(plan),
		)
	}, nil
}

func newVertexConfig(projectID, region string) (vertexConfig, error) {
	if strings.TrimSpace(projectID) == "" {
		return vertexConfig{}, fmt.Errorf("%w: Vertex project ID cannot be empty", ErrInvalidAdapter)
	}
	if !isVertexResourceComponent(projectID) {
		return vertexConfig{}, fmt.Errorf("%w: Vertex project ID must contain only lowercase letters, digits, or hyphens", ErrInvalidAdapter)
	}
	if strings.TrimSpace(region) == "" {
		return vertexConfig{}, fmt.Errorf("%w: Vertex region cannot be empty", ErrInvalidAdapter)
	}
	if !isVertexResourceComponent(region) {
		return vertexConfig{}, fmt.Errorf("%w: Vertex region must contain only lowercase letters, digits, or hyphens", ErrInvalidAdapter)
	}
	return vertexConfig{projectID: projectID, region: region}, nil
}

func isVertexResourceComponent(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validateVertexPlan(plan modelrouter.Plan, protocol modelrouter.Protocol) error {
	if err := validateBindingPlan(plan, protocol); err != nil {
		return err
	}
	if plan.Provider() != modelrouter.ProviderVertexAI {
		return fmt.Errorf("%w: Vertex adapter received provider %q", ErrInvalidBinding, plan.Provider())
	}
	attribution := plan.Attribution()
	if attribution.ProviderName != "gcp.vertex_ai" || attribution.LegacySystem != agenttrace.SystemGoogleVertex {
		return fmt.Errorf("%w: Vertex route attribution must use provider name %q and legacy system %q, got %q and %q",
			ErrInvalidBinding, "gcp.vertex_ai", agenttrace.SystemGoogleVertex, attribution.ProviderName, attribution.LegacySystem)
	}
	return nil
}

func (c vertexConfig) resourceLabels(plan modelrouter.Plan) map[string]string {
	return map[string]string{
		"projectID":  c.projectID,
		"region":     c.region,
		"model_name": strings.ToLower(plan.ProviderModelID()),
	}
}

func (c vertexConfig) openAIBaseURL() string {
	if c.region == "global" {
		return fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1beta1/projects/%s/locations/global/endpoints/openapi",
			c.projectID,
		)
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/endpoints/openapi",
		c.region, c.projectID, c.region,
	)
}
