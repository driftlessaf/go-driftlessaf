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

// newOpenAICompatAgent creates an agent using Vertex AI's OpenAI-compatible endpoint.
// Model names use publisher/model format (e.g. "google/gemini-2.5-pro").
func newOpenAICompatAgent[Req promptbuilder.Bindable, Resp, CB any](
	ctx context.Context,
	projectID, region, model string,
	config Config[Resp, CB],
) (Agent[Req, Resp, CB], error) {
	// Validate config before making network calls.
	if config.UserPrompt == nil {
		return nil, fmt.Errorf("creating OpenAI-compatible executor: prompt cannot be nil")
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

	executorOpts := []openaiexecutor.Option[Req, Resp]{
		openaiexecutor.WithModel[Req, Resp](model),
		openaiexecutor.WithTemperature[Req, Resp](0.2),
		openaiexecutor.WithMaxTokens[Req, Resp](32768),
		openaiexecutor.WithSubmitResultProvider[Req, Resp](submitresult.OpenAIToolForResponse[Resp]),
		openaiexecutor.WithResourceLabels[Req, Resp](map[string]string{
			"projectID":  projectID,
			"region":     region,
			"model_name": strings.ToLower(model),
		}),
	}

	if config.MaxTurns > 0 {
		executorOpts = append(executorOpts, openaiexecutor.WithMaxTurns[Req, Resp](config.MaxTurns))
	}

	if config.SystemInstructions != nil {
		executorOpts = append(executorOpts, openaiexecutor.WithSystemInstructions[Req, Resp](config.SystemInstructions))
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

func (a *openAICompatAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building tools: %w", err)
	}
	return a.executor.Execute(ctx, request, openaistool.Map(tools))
}
