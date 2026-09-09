/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"strings"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

// NewBedrockOpenAIResponsesAdapter constructs a native Responses adapter using
// the shared AWS Bedrock Runtime SigV4 transport. cfg selects an explicit region
// and optional IAM Identity Center profile; otherwise it uses web identity.
// Credentials are discovered only after a validated Bedrock route selects this
// adapter. No OpenAI API key or ambient OPENAI_* configuration is consumed.
//
// The adapter does not register routes, check entitlement or retention, or run
// inference. The application must verify those prerequisites before execution.
func NewBedrockOpenAIResponsesAdapter(cfg awsauth.Config) (OpenAIResponsesAdapter, error) {
	return newBedrockOpenAIResponsesAdapter(cfg, bedrockruntime.New)
}

func newBedrockOpenAIResponsesAdapter(cfg awsauth.Config, newTransport bedrockRuntimeFactory) (OpenAIResponsesAdapter, error) {
	if !bedrockruntime.ValidRegion(cfg.Region) {
		return nil, fmt.Errorf("%w: invalid Bedrock AWS region", ErrInvalidAdapter)
	}
	if cfg.Profile != strings.TrimSpace(cfg.Profile) {
		return nil, fmt.Errorf("%w: Bedrock AWS profile must not contain leading or trailing whitespace", ErrInvalidAdapter)
	}
	if newTransport == nil {
		return nil, fmt.Errorf("%w: Bedrock Runtime factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (OpenAIResponsesBinding, error) {
		if err := validateProviderPlan(plan, modelrouter.ProtocolOpenAIResponses,
			modelrouter.ProviderAWSBedrock, agenttrace.SystemBedrock, agenttrace.SystemBedrock); err != nil {
			return OpenAIResponsesBinding{}, err
		}
		transport, err := newTransport(ctx, cfg)
		if err != nil {
			return OpenAIResponsesBinding{}, fmt.Errorf("creating Bedrock Runtime client: %w", err)
		}
		// Service-only construction skips NewClient's ambient OPENAI_* defaults.
		// Only this transport can discover credentials and sign HTTP requests.
		return NewOpenAIResponsesBinding(plan, responses.NewResponseService(
			option.WithBaseURL(transport.Endpoint()+"/openai/v1/"),
			option.WithHTTPClient(transport),
			option.WithMaxRetries(0),
			option.WithJSONSet("store", false),
		), nil)
	}, nil
}
