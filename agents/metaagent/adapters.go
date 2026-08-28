/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

var (
	// ErrInvalidAdapter identifies an invalid adapter registration.
	ErrInvalidAdapter = errors.New("invalid model adapter")
	// ErrDuplicateAdapter identifies multiple adapters registered for the same
	// provider and protocol.
	ErrDuplicateAdapter = errors.New("duplicate model adapter")
	// ErrAdapterNotFound identifies a resolved provider and protocol with no
	// registered adapter.
	ErrAdapterNotFound = errors.New("model adapter not found")
	// ErrInvalidBinding identifies an invalid or substituted protocol binding.
	ErrInvalidBinding = errors.New("invalid model binding")
)

// GoogleGenAIAdapter constructs a Google Gen AI SDK binding for a resolved
// plan. The adapter may capture provider configuration and credentials, but
// must return the same plan it receives.
type GoogleGenAIAdapter func(context.Context, modelrouter.Plan) (GoogleGenAIBinding, error)

// AnthropicMessagesAdapter constructs an Anthropic Messages SDK binding for a
// resolved plan. The adapter may capture provider configuration and
// credentials, but must return the same plan it receives.
type AnthropicMessagesAdapter func(context.Context, modelrouter.Plan) (AnthropicMessagesBinding, error)

// OpenAIChatCompletionsAdapter constructs an OpenAI Chat Completions SDK
// binding for a resolved plan. The adapter may capture provider configuration
// and credentials, but must return the same plan it receives.
type OpenAIChatCompletionsAdapter func(context.Context, modelrouter.Plan) (OpenAIChatCompletionsBinding, error)

// GoogleGenAIBinding carries the typed client and non-secret provider inputs
// needed by the Google executor. Its Plan remains the authority for model and
// attribution values.
type GoogleGenAIBinding struct {
	plan           modelrouter.Plan
	client         *genai.Client
	resourceLabels map[string]string
	initialized    bool
}

// NewGoogleGenAIBinding validates plan and constructs a Google Gen AI binding.
func NewGoogleGenAIBinding(plan modelrouter.Plan, client *genai.Client, resourceLabels map[string]string) (GoogleGenAIBinding, error) {
	if err := validateBindingPlan(plan, modelrouter.ProtocolGoogleGenAI); err != nil {
		return GoogleGenAIBinding{}, err
	}
	if client == nil {
		return GoogleGenAIBinding{}, fmt.Errorf("%w: Google Gen AI client is nil", ErrInvalidBinding)
	}
	return GoogleGenAIBinding{
		plan:           plan,
		client:         client,
		resourceLabels: maps.Clone(resourceLabels),
		initialized:    true,
	}, nil
}

// Plan returns the immutable route plan carried by b.
func (b GoogleGenAIBinding) Plan() modelrouter.Plan { return b.plan }

// Client returns the typed Google Gen AI client carried by b.
func (b GoogleGenAIBinding) Client() *genai.Client { return b.client }

// ResourceLabels returns a copy of the provider resource labels carried by b.
func (b GoogleGenAIBinding) ResourceLabels() map[string]string {
	return maps.Clone(b.resourceLabels)
}

// AnthropicMessagesBinding carries the typed Messages service and non-secret
// provider inputs needed by the Claude executor. Its Plan remains the
// authority for model and attribution values.
type AnthropicMessagesBinding struct {
	plan           modelrouter.Plan
	messages       anthropic.MessageService
	resourceLabels map[string]string
	initialized    bool
}

// NewAnthropicMessagesBinding validates plan and constructs an Anthropic
// Messages binding.
func NewAnthropicMessagesBinding(plan modelrouter.Plan, messages anthropic.MessageService, resourceLabels map[string]string) (AnthropicMessagesBinding, error) {
	if err := validateBindingPlan(plan, modelrouter.ProtocolAnthropicMessages); err != nil {
		return AnthropicMessagesBinding{}, err
	}
	return AnthropicMessagesBinding{
		plan:           plan,
		messages:       messages,
		resourceLabels: maps.Clone(resourceLabels),
		initialized:    true,
	}, nil
}

// Plan returns the immutable route plan carried by b.
func (b AnthropicMessagesBinding) Plan() modelrouter.Plan { return b.plan }

// Messages returns the typed Anthropic Messages service carried by b.
func (b AnthropicMessagesBinding) Messages() anthropic.MessageService { return b.messages }

// ResourceLabels returns a copy of the provider resource labels carried by b.
func (b AnthropicMessagesBinding) ResourceLabels() map[string]string {
	return maps.Clone(b.resourceLabels)
}

// OpenAIChatCompletionsBinding carries the typed client and non-secret
// provider inputs needed by the OpenAI executor. Its Plan remains the
// authority for model and attribution values.
type OpenAIChatCompletionsBinding struct {
	plan                modelrouter.Plan
	client              openai.Client
	tokenLimitParameter openaiexecutor.TokenLimitParameter
	resourceLabels      map[string]string
	initialized         bool
}

// NewOpenAIChatCompletionsBinding validates plan and constructs an OpenAI Chat
// Completions binding.
func NewOpenAIChatCompletionsBinding(
	plan modelrouter.Plan,
	client openai.Client,
	tokenLimitParameter openaiexecutor.TokenLimitParameter,
	resourceLabels map[string]string,
) (OpenAIChatCompletionsBinding, error) {
	if err := validateBindingPlan(plan, modelrouter.ProtocolOpenAIChatCompletions); err != nil {
		return OpenAIChatCompletionsBinding{}, err
	}
	switch tokenLimitParameter {
	case openaiexecutor.TokenLimitMaxCompletionTokens, openaiexecutor.TokenLimitMaxTokens:
	default:
		return OpenAIChatCompletionsBinding{}, fmt.Errorf("%w: unsupported OpenAI token-limit parameter %q", ErrInvalidBinding, tokenLimitParameter)
	}
	return OpenAIChatCompletionsBinding{
		plan:                plan,
		client:              client,
		tokenLimitParameter: tokenLimitParameter,
		resourceLabels:      maps.Clone(resourceLabels),
		initialized:         true,
	}, nil
}

// Plan returns the immutable route plan carried by b.
func (b OpenAIChatCompletionsBinding) Plan() modelrouter.Plan { return b.plan }

// Client returns the typed OpenAI client carried by b.
func (b OpenAIChatCompletionsBinding) Client() openai.Client { return b.client }

// TokenLimitParameter returns the request field used for output-token limits.
func (b OpenAIChatCompletionsBinding) TokenLimitParameter() openaiexecutor.TokenLimitParameter {
	return b.tokenLimitParameter
}

// ResourceLabels returns a copy of the provider resource labels carried by b.
func (b OpenAIChatCompletionsBinding) ResourceLabels() map[string]string {
	return maps.Clone(b.resourceLabels)
}

// GoogleGenAIRegistration registers one provider adapter in a Google Gen AI
// adapter registry.
type GoogleGenAIRegistration struct {
	Provider modelrouter.Provider
	Adapter  GoogleGenAIAdapter
}

// GoogleGenAIAdapterRegistry is an immutable provider-keyed registry whose
// protocol is fixed to Google Gen AI by its type.
type GoogleGenAIAdapterRegistry struct {
	adapters map[modelrouter.Provider]GoogleGenAIAdapter
}

// NewGoogleGenAIAdapterRegistry validates registrations in declaration order.
func NewGoogleGenAIAdapterRegistry(registrations ...GoogleGenAIRegistration) (*GoogleGenAIAdapterRegistry, error) {
	registry := &GoogleGenAIAdapterRegistry{adapters: make(map[modelrouter.Provider]GoogleGenAIAdapter, len(registrations))}
	firstIndex := make(map[modelrouter.Provider]int, len(registrations))
	for i, registration := range registrations {
		if err := registration.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("registration %d: %w: provider: %w", i, ErrInvalidAdapter, err)
		}
		if registration.Adapter == nil {
			return nil, fmt.Errorf("registration %d: %w: Google Gen AI adapter is nil", i, ErrInvalidAdapter)
		}
		if first, ok := firstIndex[registration.Provider]; ok {
			return nil, fmt.Errorf("registration %d: %w for provider %q and protocol %q (first registered at registration %d)",
				i, ErrDuplicateAdapter, registration.Provider, modelrouter.ProtocolGoogleGenAI, first)
		}
		firstIndex[registration.Provider] = i
		registry.adapters[registration.Provider] = registration.Adapter
	}
	return registry, nil
}

func (r *GoogleGenAIAdapterRegistry) lookup(provider modelrouter.Provider) (GoogleGenAIAdapter, error) {
	if r != nil {
		if adapter, ok := r.adapters[provider]; ok {
			return adapter, nil
		}
	}
	return nil, adapterNotFound(provider, modelrouter.ProtocolGoogleGenAI)
}

// AnthropicMessagesRegistration registers one provider adapter in an
// Anthropic Messages adapter registry.
type AnthropicMessagesRegistration struct {
	Provider modelrouter.Provider
	Adapter  AnthropicMessagesAdapter
}

// AnthropicMessagesAdapterRegistry is an immutable provider-keyed registry
// whose protocol is fixed to Anthropic Messages by its type.
type AnthropicMessagesAdapterRegistry struct {
	adapters map[modelrouter.Provider]AnthropicMessagesAdapter
}

// NewAnthropicMessagesAdapterRegistry validates registrations in declaration
// order.
func NewAnthropicMessagesAdapterRegistry(registrations ...AnthropicMessagesRegistration) (*AnthropicMessagesAdapterRegistry, error) {
	registry := &AnthropicMessagesAdapterRegistry{adapters: make(map[modelrouter.Provider]AnthropicMessagesAdapter, len(registrations))}
	firstIndex := make(map[modelrouter.Provider]int, len(registrations))
	for i, registration := range registrations {
		if err := registration.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("registration %d: %w: provider: %w", i, ErrInvalidAdapter, err)
		}
		if registration.Adapter == nil {
			return nil, fmt.Errorf("registration %d: %w: Anthropic Messages adapter is nil", i, ErrInvalidAdapter)
		}
		if first, ok := firstIndex[registration.Provider]; ok {
			return nil, fmt.Errorf("registration %d: %w for provider %q and protocol %q (first registered at registration %d)",
				i, ErrDuplicateAdapter, registration.Provider, modelrouter.ProtocolAnthropicMessages, first)
		}
		firstIndex[registration.Provider] = i
		registry.adapters[registration.Provider] = registration.Adapter
	}
	return registry, nil
}

func (r *AnthropicMessagesAdapterRegistry) lookup(provider modelrouter.Provider) (AnthropicMessagesAdapter, error) {
	if r != nil {
		if adapter, ok := r.adapters[provider]; ok {
			return adapter, nil
		}
	}
	return nil, adapterNotFound(provider, modelrouter.ProtocolAnthropicMessages)
}

// OpenAIChatCompletionsRegistration registers one provider adapter in an
// OpenAI Chat Completions adapter registry.
type OpenAIChatCompletionsRegistration struct {
	Provider modelrouter.Provider
	Adapter  OpenAIChatCompletionsAdapter
}

// OpenAIChatCompletionsAdapterRegistry is an immutable provider-keyed registry
// whose protocol is fixed to OpenAI Chat Completions by its type.
type OpenAIChatCompletionsAdapterRegistry struct {
	adapters map[modelrouter.Provider]OpenAIChatCompletionsAdapter
}

// NewOpenAIChatCompletionsAdapterRegistry validates registrations in
// declaration order.
func NewOpenAIChatCompletionsAdapterRegistry(registrations ...OpenAIChatCompletionsRegistration) (*OpenAIChatCompletionsAdapterRegistry, error) {
	registry := &OpenAIChatCompletionsAdapterRegistry{adapters: make(map[modelrouter.Provider]OpenAIChatCompletionsAdapter, len(registrations))}
	firstIndex := make(map[modelrouter.Provider]int, len(registrations))
	for i, registration := range registrations {
		if err := registration.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("registration %d: %w: provider: %w", i, ErrInvalidAdapter, err)
		}
		if registration.Adapter == nil {
			return nil, fmt.Errorf("registration %d: %w: OpenAI Chat Completions adapter is nil", i, ErrInvalidAdapter)
		}
		if first, ok := firstIndex[registration.Provider]; ok {
			return nil, fmt.Errorf("registration %d: %w for provider %q and protocol %q (first registered at registration %d)",
				i, ErrDuplicateAdapter, registration.Provider, modelrouter.ProtocolOpenAIChatCompletions, first)
		}
		firstIndex[registration.Provider] = i
		registry.adapters[registration.Provider] = registration.Adapter
	}
	return registry, nil
}

func (r *OpenAIChatCompletionsAdapterRegistry) lookup(provider modelrouter.Provider) (OpenAIChatCompletionsAdapter, error) {
	if r != nil {
		if adapter, ok := r.adapters[provider]; ok {
			return adapter, nil
		}
	}
	return nil, adapterNotFound(provider, modelrouter.ProtocolOpenAIChatCompletions)
}

func adapterNotFound(provider modelrouter.Provider, protocol modelrouter.Protocol) error {
	return fmt.Errorf("%w for provider %q and protocol %q", ErrAdapterNotFound, provider, protocol)
}

func validateBindingPlan(plan modelrouter.Plan, protocol modelrouter.Protocol) error {
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBinding, err)
	}
	if plan.Protocol() != protocol {
		return fmt.Errorf("%w: plan protocol is %q, want %q", ErrInvalidBinding, plan.Protocol(), protocol)
	}
	return nil
}

func validateReturnedPlan(want, got modelrouter.Plan) error {
	if err := got.Validate(); err != nil {
		return fmt.Errorf("%w: adapter returned an unresolved plan: %w", ErrInvalidBinding, err)
	}
	if want.SameResolution(got) {
		return nil
	}
	return fmt.Errorf("%w: adapter substituted the resolved route plan", ErrInvalidBinding)
}
