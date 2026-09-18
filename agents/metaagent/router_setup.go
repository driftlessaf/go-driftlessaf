/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"fmt"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// AdapterRegistrations groups typed registrations for application startup.
// Each adapter captures its provider's explicit configuration. Empty slices
// register no adapters for that protocol.
type AdapterRegistrations struct {
	GoogleGenAI           []GoogleGenAIRegistration
	AnthropicMessages     []AnthropicMessagesRegistration
	OpenAIChatCompletions []OpenAIChatCompletionsRegistration
	OpenAIResponses       []OpenAIResponsesRegistration
}

// NewRouterWithAdapters validates typed registrations and constructs their
// registries in one call. It does not invoke adapters or load credentials.
// Routes and registrations are copied; callers can reuse or modify the input
// slices after construction. As with NewRouter, concurrent binding requires
// the registered adapter functions to be safe for concurrent use.
func NewRouterWithAdapters(routes *modelrouter.Registry, registrations AdapterRegistrations) (*Router, error) {
	if routes == nil {
		return nil, fmt.Errorf("%w: route registry is nil", ErrInvalidRouter)
	}
	google, err := NewGoogleGenAIAdapterRegistry(registrations.GoogleGenAI...)
	if err != nil {
		return nil, err
	}
	anthropic, err := NewAnthropicMessagesAdapterRegistry(registrations.AnthropicMessages...)
	if err != nil {
		return nil, err
	}
	chat, err := NewOpenAIChatCompletionsAdapterRegistry(registrations.OpenAIChatCompletions...)
	if err != nil {
		return nil, err
	}
	responses, err := NewOpenAIResponsesAdapterRegistry(registrations.OpenAIResponses...)
	if err != nil {
		return nil, err
	}
	return NewRouter(routes, AdapterRegistries{
		GoogleGenAI: google, AnthropicMessages: anthropic,
		OpenAIChatCompletions: chat, OpenAIResponses: responses,
	})
}
