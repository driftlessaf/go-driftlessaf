/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// NewBedrockRuntimeRouter registers the Bedrock Runtime adapters needed by exact
// route declarations in one AWS region and credential configuration. It supports
// Anthropic Messages, OpenAI Chat Completions, and OpenAI Responses. It does not
// infer model IDs, load credentials, or check account-specific model access.
// The returned router is safe for concurrent use. Mantle requires a separate
// router built with NewBedrockAnthropicMessagesAdapter and NewRouterWithAdapters.
func NewBedrockRuntimeRouter(cfg awsauth.Config, routes ...modelrouter.Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("%w: Bedrock Runtime router requires at least one declared route", ErrInvalidRouter)
	}
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		return nil, err
	}
	var adapters AdapterRegistrations
	for _, route := range routes {
		plan, err := registry.Resolve(route.Selection)
		if err != nil {
			return nil, err
		}
		if err := validateProviderPlan(plan, route.Protocol, modelrouter.ProviderAWSBedrock, agenttrace.SystemBedrock, agenttrace.SystemBedrock); err != nil {
			return nil, err
		}
		switch route.Protocol {
		case modelrouter.ProtocolAnthropicMessages:
			if adapters.AnthropicMessages != nil {
				continue
			}
			adapter, err := NewBedrockRuntimeAnthropicMessagesAdapter(cfg)
			if err != nil {
				return nil, err
			}
			adapters.AnthropicMessages = []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter}}
		case modelrouter.ProtocolOpenAIChatCompletions:
			if adapters.OpenAIChatCompletions != nil {
				continue
			}
			adapter, err := NewBedrockOpenAIChatCompletionsAdapter(cfg)
			if err != nil {
				return nil, err
			}
			adapters.OpenAIChatCompletions = []OpenAIChatCompletionsRegistration{{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter}}
		case modelrouter.ProtocolOpenAIResponses:
			if adapters.OpenAIResponses != nil {
				continue
			}
			adapter, err := NewBedrockOpenAIResponsesAdapter(cfg)
			if err != nil {
				return nil, err
			}
			adapters.OpenAIResponses = []OpenAIResponsesRegistration{{Provider: modelrouter.ProviderAWSBedrock, Adapter: adapter}}
		default:
			return nil, fmt.Errorf("%w: Bedrock Runtime has no built-in adapter for protocol %q", ErrInvalidRouter, route.Protocol)
		}
	}
	return NewRouterWithAdapters(registry, adapters)
}
