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

	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"chainguard.dev/driftlessaf/agents/submitresult"
	"chainguard.dev/driftlessaf/agents/toolcall/openaistool"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// openAICompatAgent implements Agent using the OpenAI-compatible API (e.g. Vertex AI partner models).
type openAICompatAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor openaiexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
}

// OpenAICompatibleProvider configures an external OpenAI-compatible backend.
// APIKey is used only to construct the authenticated client; it is not added
// to logs, traces, metrics, or resource labels.
type OpenAICompatibleProvider struct {
	BaseURL             string
	APIKey              string
	Provider            openaiexecutor.Provider
	TokenLimitParameter openaiexecutor.TokenLimitParameter
}

// NewOpenAICompatible creates an agent for an explicitly configured external
// OpenAI-compatible backend. Callers must provide the serving provider and
// token-limit parameter rather than relying on model-name routing.
func NewOpenAICompatible[Req promptbuilder.Bindable, Resp, CB any](
	provider OpenAICompatibleProvider,
	model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	if strings.TrimSpace(provider.BaseURL) == "" {
		return nil, fmt.Errorf("creating OpenAI-compatible executor: base URL cannot be empty")
	}
	if strings.TrimSpace(provider.APIKey) == "" {
		return nil, fmt.Errorf("creating OpenAI-compatible executor: API key cannot be empty")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("creating OpenAI-compatible executor: model cannot be empty")
	}
	if err := validateOpenAICompatibleConfig(config); err != nil {
		return nil, err
	}

	client := openai.NewClient(
		option.WithBaseURL(provider.BaseURL),
		option.WithAPIKey(provider.APIKey),
	)

	return newOpenAICompatibleAgentWithClient[Req, Resp, CB](
		client,
		model,
		provider.Provider,
		provider.TokenLimitParameter,
		map[string]string{"model_name": strings.ToLower(model)},
		config,
	)
}

// newOpenAICompatAgent creates an agent using Vertex AI's OpenAI-compatible endpoint.
// Model names use publisher/model format (e.g. "google/gemini-2.5-pro").
func newOpenAICompatAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	if err := validateOpenAICompatibleConfig(config); err != nil {
		return nil, err
	}

	// Use GCP Application Default Credentials for authentication.
	// The oauth2 transport overwrites the Authorization header set by the OpenAI SDK,
	// replacing the dummy API key with a real GCP access token on each request.
	tokenSource, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("creating GCP token source: %w", err)
	}

	// The "global" region uses a different hostname than regional endpoints.
	var baseURL string
	if region == "global" {
		baseURL = fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1beta1/projects/%s/locations/global/endpoints/openapi",
			projectID,
		)
	} else {
		baseURL = fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/endpoints/openapi",
			region, projectID, region,
		)
	}

	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		// Provide a non-empty placeholder; the oauth2 transport replaces this with a real GCP token.
		option.WithAPIKey("vertex-ai-auth"),
		option.WithHTTPClient(&http.Client{
			Transport: &oauth2.Transport{Source: tokenSource},
		}),
	)

	return newOpenAICompatibleAgentWithClient[Req, Resp, CB](
		client,
		model,
		openaiexecutor.ProviderOpenAICompatible,
		openaiexecutor.TokenLimitMaxCompletionTokens,
		map[string]string{
			"projectID":  projectID,
			"region":     region,
			"model_name": strings.ToLower(model),
		},
		config,
	)
}

func newOpenAICompatibleAgentWithClient[Req promptbuilder.Bindable, Resp, CB any](
	client openai.Client,
	model string,
	provider openaiexecutor.Provider,
	tokenLimitParameter openaiexecutor.TokenLimitParameter,
	resourceLabels map[string]string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	// Build the terminal submit_result tool. The executor gates accepted
	// submissions on the configured result validators before committing them
	// as the run's final result.
	submitTool, err := submitresult.OpenAITool(submitOptions(config))
	if err != nil {
		return nil, fmt.Errorf("building submit tool: %w", err)
	}

	executorOpts := []openaiexecutor.Option[Req, Resp]{
		openaiexecutor.WithModel[Req, Resp](model),
		openaiexecutor.WithTemperature[Req, Resp](0.2),
		openaiexecutor.WithMaxTokens[Req, Resp](32768),
		openaiexecutor.WithProvider[Req, Resp](provider),
		openaiexecutor.WithTokenLimitParameter[Req, Resp](tokenLimitParameter),
		openaiexecutor.WithSubmitResultProvider[Req, Resp](func() (openaistool.SubmitMetadata[Resp], error) { return submitTool, nil }),
		openaiexecutor.WithResourceLabels[Req, Resp](resourceLabels),
	}
	for _, v := range config.ResultValidators {
		executorOpts = append(executorOpts, openaiexecutor.WithResultValidator[Req, Resp](v))
	}

	if config.MaxTurns > 0 {
		executorOpts = append(executorOpts, openaiexecutor.WithMaxTurns[Req, Resp](config.MaxTurns))
	}

	if config.ToolCallConcurrency > 0 {
		executorOpts = append(executorOpts, openaiexecutor.WithToolCallConcurrency[Req, Resp](config.ToolCallConcurrency))
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, openaiexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
	}

	// The OpenAI-compatible API has no per-block prompt-cache semantics, so
	// the suffix is simply appended to the built user prompt (see
	// openaiexecutor.WithUserPromptSuffix).
	if config.UserPromptSuffix != nil {
		executorOpts = append(executorOpts, openaiexecutor.WithUserPromptSuffix[Req, Resp](config.UserPromptSuffix))
	}

	if config.Effort != "" {
		executorOpts = append(executorOpts, openaiexecutor.WithEffort[Req, Resp](config.Effort))
	}

	exec, err := openaiexecutor.New[Req, Resp](client, config.UserPrompt, executorOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating OpenAI-compatible executor: %w", err)
	}

	return &openAICompatAgent[Req, Resp, CB]{
		executor: exec,
		config:   config,
	}, nil
}

func validateOpenAICompatibleConfig[Resp, CB any](config Config[Resp, CB]) error {
	if config.UserPrompt == nil {
		return fmt.Errorf("creating OpenAI-compatible executor: prompt cannot be nil")
	}
	// Suspend/resume is not wired for this backend yet: openaiexecutor has no
	// suspend tool option, so a set SuspendToolName would otherwise be silently
	// dropped and the advertised pause lifecycle could never fire. Fail closed
	// with a clear error until the openaiexecutor suspend/resume slice lands
	// (DEV-2247). openAICompatAgent likewise does not implement Resumer, so
	// AsResumer reports false for agents built here.
	if config.SuspendToolName != "" {
		return fmt.Errorf("suspend/resume (SuspendToolName %q) is not yet supported on the OpenAI-compatible backend; it lands with the openaiexecutor suspend/resume slice", config.SuspendToolName)
	}
	return nil
}

func (a *openAICompatAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	return a.executor.Execute(ctx, request, openaistool.Map(tools))
}
