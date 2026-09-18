/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// NewVertexRouter constructs a router for explicitly declared Vertex AI routes
// in one project and region. It registers the Google Gen AI, Anthropic Messages,
// and OpenAI Chat Completions adapters needed by those routes. It neither infers
// routes from model names nor loads credentials; NewRouted binds only the chosen
// route. The returned router is safe for concurrent use.
//
// Construct one router at application startup and share it with agent factories.
// Use a separate router for each project/region pair. For mixed-provider routing
// or custom adapter settings, use NewRouterWithAdapters with typed registrations.
func NewVertexRouter(projectID, region string, routes ...modelrouter.Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("%w: Vertex router requires at least one declared route", ErrInvalidRouter)
	}
	if _, err := newVertexConfig(projectID, region); err != nil {
		return nil, err
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
		if err := validateVertexPlan(plan, route.Protocol); err != nil {
			return nil, err
		}
		switch route.Protocol {
		case modelrouter.ProtocolGoogleGenAI:
			if adapters.GoogleGenAI != nil {
				continue
			}
			adapter, err := NewVertexGoogleGenAIAdapter(projectID, region)
			if err != nil {
				return nil, err
			}
			adapters.GoogleGenAI = []GoogleGenAIRegistration{{Provider: modelrouter.ProviderVertexAI, Adapter: adapter}}
		case modelrouter.ProtocolAnthropicMessages:
			if adapters.AnthropicMessages != nil {
				continue
			}
			adapter, err := NewVertexAnthropicMessagesAdapter(projectID, region)
			if err != nil {
				return nil, err
			}
			adapters.AnthropicMessages = []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderVertexAI, Adapter: adapter}}
		case modelrouter.ProtocolOpenAIChatCompletions:
			if adapters.OpenAIChatCompletions != nil {
				continue
			}
			adapter, err := NewVertexOpenAIChatCompletionsAdapter(projectID, region)
			if err != nil {
				return nil, err
			}
			adapters.OpenAIChatCompletions = []OpenAIChatCompletionsRegistration{{Provider: modelrouter.ProviderVertexAI, Adapter: adapter}}
		default:
			return nil, fmt.Errorf("%w: Vertex router has no built-in adapter for protocol %q; use NewRouterWithAdapters with an explicit adapter", ErrInvalidRouter, route.Protocol)
		}
	}
	return NewRouterWithAdapters(registry, adapters)
}
