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
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// NewBedrockRuntimeAnthropicMessagesAdapter binds Anthropic Messages to the
// regional Bedrock Runtime endpoint through the shared SigV4 transport.
// Credentials are loaded only when a validated Bedrock plan selects the adapter.
// It does not register routes or verify model access or retention eligibility.
// NewBedrockAnthropicMessagesAdapter remains the separate Mantle constructor.
func NewBedrockRuntimeAnthropicMessagesAdapter(cfg awsauth.Config) (AnthropicMessagesAdapter, error) {
	return newBedrockRuntimeAnthropicMessagesAdapter(cfg, bedrockruntime.New)
}

func newBedrockRuntimeAnthropicMessagesAdapter(cfg awsauth.Config, newTransport bedrockRuntimeFactory) (AnthropicMessagesAdapter, error) {
	if !bedrockruntime.ValidRegion(cfg.Region) {
		return nil, fmt.Errorf("%w: invalid Bedrock AWS region", ErrInvalidAdapter)
	}
	if cfg.Profile != strings.TrimSpace(cfg.Profile) {
		return nil, fmt.Errorf("%w: Bedrock AWS profile must not contain leading or trailing whitespace", ErrInvalidAdapter)
	}
	if newTransport == nil {
		return nil, fmt.Errorf("%w: Bedrock Runtime factory is nil", ErrInvalidAdapter)
	}
	return func(ctx context.Context, plan modelrouter.Plan) (AnthropicMessagesBinding, error) {
		if err := validateProviderPlan(plan, modelrouter.ProtocolAnthropicMessages,
			modelrouter.ProviderAWSBedrock, agenttrace.SystemBedrock, agenttrace.SystemBedrock); err != nil {
			return AnthropicMessagesBinding{}, err
		}
		transport, err := newTransport(ctx, cfg)
		if err != nil {
			return AnthropicMessagesBinding{}, fmt.Errorf("creating Bedrock Runtime client: %w", err)
		}
		return NewAnthropicMessagesBinding(plan, anthropic.NewMessageService(
			option.WithoutEnvironmentDefaults(),
			option.WithBaseURL(transport.Endpoint()+"/anthropic/"),
			option.WithHTTPClient(transport),
			// The executor owns retries and records them in the trace.
			option.WithMaxRetries(0),
		), map[string]string{"region": cfg.Region})
	}, nil
}
