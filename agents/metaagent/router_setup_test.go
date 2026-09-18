/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"google.golang.org/genai"
)

func TestNewRouterWithAdaptersSelectsOnlyRequestedBinding(t *testing.T) {
	t.Parallel()
	for _, protocol := range []modelrouter.Protocol{modelrouter.ProtocolGoogleGenAI, modelrouter.ProtocolAnthropicMessages, modelrouter.ProtocolOpenAIChatCompletions, modelrouter.ProtocolOpenAIResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			const provider modelrouter.Provider = "explicit-test-provider"
			id := "example/model"
			if protocol == modelrouter.ProtocolGoogleGenAI {
				id = "gemini-2.5-flash"
			}
			if protocol == modelrouter.ProtocolAnthropicMessages {
				id = "claude-sonnet-4-6"
			}
			route := routedTestRoute(modelrouter.Selection{Provider: provider, LogicalModel: id}, protocol, id)
			calls := make(map[modelrouter.Protocol]int)
			registrations := AdapterRegistrations{
				GoogleGenAI: []GoogleGenAIRegistration{{Provider: provider, Adapter: func(_ context.Context, p modelrouter.Plan) (GoogleGenAIBinding, error) {
					calls[modelrouter.ProtocolGoogleGenAI]++
					return NewGoogleGenAIBinding(p, &genai.Client{}, nil)
				}}},
				AnthropicMessages: []AnthropicMessagesRegistration{{Provider: provider, Adapter: func(_ context.Context, p modelrouter.Plan) (AnthropicMessagesBinding, error) {
					calls[modelrouter.ProtocolAnthropicMessages]++
					return NewAnthropicMessagesBinding(p, anthropic.NewMessageService(), nil)
				}}},
				OpenAIChatCompletions: []OpenAIChatCompletionsRegistration{{Provider: provider, Adapter: func(_ context.Context, p modelrouter.Plan) (OpenAIChatCompletionsBinding, error) {
					calls[modelrouter.ProtocolOpenAIChatCompletions]++
					return NewOpenAIChatCompletionsBinding(p, openai.Client{}, "max_tokens", nil)
				}}},
				OpenAIResponses: []OpenAIResponsesRegistration{{Provider: provider, Adapter: func(_ context.Context, p modelrouter.Plan) (OpenAIResponsesBinding, error) {
					calls[modelrouter.ProtocolOpenAIResponses]++
					return NewOpenAIResponsesBinding(p, responses.ResponseService{}, nil)
				}}},
			}
			router, err := NewRouterWithAdapters(mustRouteRegistry(t, route), registrations)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatal("router setup invoked an adapter")
			}
			// Mutating the input registrations must not change the registered adapters.
			registrations.GoogleGenAI[0].Adapter = nil
			registrations.AnthropicMessages[0].Adapter = nil
			registrations.OpenAIChatCompletions[0].Adapter = nil
			registrations.OpenAIResponses[0].Adapter = nil
			if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, routedTestConfig(t)); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[protocol] != 1 {
				t.Errorf("adapter calls: got = %v, want = only %s once", calls, protocol)
			}
		})
	}
}

func TestNewRouterWithAdaptersRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()
	route := vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")
	registry := mustRouteRegistry(t, route)
	if _, err := NewRouterWithAdapters(nil, AdapterRegistrations{}); !errors.Is(err, ErrInvalidRouter) {
		t.Errorf("nil routes: got = %v, want = ErrInvalidRouter", err)
	}
	for _, registrations := range []AdapterRegistrations{
		{GoogleGenAI: []GoogleGenAIRegistration{{Provider: modelrouter.ProviderVertexAI}}},
		{AnthropicMessages: []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderVertexAI}}},
		{OpenAIChatCompletions: []OpenAIChatCompletionsRegistration{{Provider: modelrouter.ProviderVertexAI}}},
		{OpenAIResponses: []OpenAIResponsesRegistration{{Provider: modelrouter.ProviderVertexAI}}},
	} {
		if _, err := NewRouterWithAdapters(registry, registrations); !errors.Is(err, ErrInvalidAdapter) {
			t.Errorf("nil adapter: got = %v, want = ErrInvalidAdapter", err)
		}
	}
	adapter, err := NewVertexGoogleGenAIAdapter("test-project", "global")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := GoogleGenAIRegistration{Provider: modelrouter.ProviderVertexAI, Adapter: adapter}
	if _, err := NewRouterWithAdapters(registry, AdapterRegistrations{GoogleGenAI: []GoogleGenAIRegistration{duplicate, duplicate}}); !errors.Is(err, ErrDuplicateAdapter) {
		t.Errorf("duplicate: got = %v, want = ErrDuplicateAdapter", err)
	}
}
