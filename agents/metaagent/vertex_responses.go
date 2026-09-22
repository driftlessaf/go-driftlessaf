/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"net/http"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// NewVertexOpenAIResponsesAdapter constructs a Vertex AI Responses adapter.
// Application Default Credentials are discovered only when a validated route
// binds the service. The transport refreshes OAuth tokens for each request;
// credentials and ambient OPENAI_* settings never enter routes or plans.
func NewVertexOpenAIResponsesAdapter(projectID, region string) (OpenAIResponsesAdapter, error) {
	return newVertexOpenAIResponsesAdapter(projectID, region, google.DefaultTokenSource)
}

func newVertexOpenAIResponsesAdapter(projectID, region string, newTokenSource vertexTokenSourceFactory) (OpenAIResponsesAdapter, error) {
	config, err := newVertexConfig(projectID, region)
	if err != nil {
		return nil, err
	}
	if newTokenSource == nil {
		return nil, fmt.Errorf("%w: Vertex token-source factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (OpenAIResponsesBinding, error) {
		if err := validateVertexPlan(plan, modelrouter.ProtocolOpenAIResponses); err != nil {
			return OpenAIResponsesBinding{}, err
		}
		tokenSource, err := newTokenSource(ctx, vertexCloudPlatformScope)
		if err != nil {
			return OpenAIResponsesBinding{}, fmt.Errorf("creating GCP token source: %w", err)
		}
		if tokenSource == nil {
			return OpenAIResponsesBinding{}, fmt.Errorf("creating GCP token source: returned nil token source")
		}
		host := "aiplatform.googleapis.com"
		// Vertex multi-regions use jurisdictional endpoints rather than the
		// regional hostname prefix. See https://cloud.google.com/vertex-ai/generative-ai/docs/learn/locations.
		switch config.region {
		case "global":
		case "us", "eu":
			host = "aiplatform." + config.region + ".rep.googleapis.com"
		default:
			host = config.region + "-" + host
		}
		return NewOpenAIResponsesBinding(plan, responses.NewResponseService(
			option.WithBaseURL(fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/endpoints/openapi/", host, config.projectID, config.region)),
			option.WithHTTPClient(&http.Client{
				Transport: &oauth2.Transport{Source: tokenSource},
				// OAuth transports attach credentials to every request, including redirects.
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}),
			option.WithMaxRetries(0),
			option.WithJSONSet("store", false),
		), config.resourceLabels(plan))
	}, nil
}
