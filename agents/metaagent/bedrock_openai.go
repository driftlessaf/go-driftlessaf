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
	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type bedrockRuntimeFactory func(context.Context, awsauth.Config) (bedrockruntime.Client, error)

// NewBedrockOpenAIChatCompletionsAdapter constructs an AWS Bedrock Runtime
// adapter for the OpenAI Chat Completions protocol. It uses temporary AWS
// credentials through the shared SigV4 transport, not OpenAI API keys.
// Credential discovery occurs only when a validated Bedrock plan selects the
// adapter. cfg selects an explicit region and optional IAM Identity Center
// profile; an empty profile uses the awsauth web-identity path.
//
// This adapter initializes only the client's Chat service. It does not register
// routes or verify account access to the plan's exact provider model ID.
func NewBedrockOpenAIChatCompletionsAdapter(cfg awsauth.Config) (OpenAIChatCompletionsAdapter, error) {
	return newBedrockOpenAIChatCompletionsAdapter(cfg, bedrockruntime.New)
}

func newBedrockOpenAIChatCompletionsAdapter(cfg awsauth.Config, newTransport bedrockRuntimeFactory) (OpenAIChatCompletionsAdapter, error) {
	if cfg.Region == "" || cfg.Region != strings.TrimSpace(cfg.Region) || !isAWSRegion(cfg.Region) {
		return nil, fmt.Errorf("%w: invalid Bedrock AWS region", ErrInvalidAdapter)
	}
	if cfg.Profile != strings.TrimSpace(cfg.Profile) {
		return nil, fmt.Errorf("%w: Bedrock AWS profile must not contain leading or trailing whitespace", ErrInvalidAdapter)
	}
	if newTransport == nil {
		return nil, fmt.Errorf("%w: Bedrock Runtime factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
		if err := validateBindingPlan(plan, modelrouter.ProtocolOpenAIChatCompletions); err != nil {
			return OpenAIChatCompletionsBinding{}, err
		}
		if plan.Provider() != modelrouter.ProviderAWSBedrock {
			return OpenAIChatCompletionsBinding{}, fmt.Errorf("%w: Bedrock Chat Completions adapter received provider %q", ErrInvalidBinding, plan.Provider())
		}
		attribution := plan.Attribution()
		if attribution.ProviderName != agenttrace.SystemBedrock || attribution.LegacySystem != agenttrace.SystemBedrock {
			return OpenAIChatCompletionsBinding{}, fmt.Errorf("%w: Bedrock Chat Completions attribution must use the Bedrock provider and legacy system", ErrInvalidBinding)
		}
		transport, err := newTransport(ctx, cfg)
		if err != nil {
			return OpenAIChatCompletionsBinding{}, fmt.Errorf("creating Bedrock Runtime client: %w", err)
		}
		// NewClient reads OPENAI_* environment variables, including secrets.
		// Construct only the required service to avoid capturing those defaults.
		return NewOpenAIChatCompletionsBinding(plan, openai.Client{Chat: openai.NewChatService(
			option.WithBaseURL(transport.Endpoint()+"/openai/v1/"),
			option.WithHTTPClient(transport),
			// The executor owns retries and records them in the trace.
			option.WithMaxRetries(0),
			option.WithJSONSet("store", false),
		)}, openaiexecutor.TokenLimitMaxCompletionTokens, nil)
	}, nil
}
