/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"chainguard.dev/driftlessaf/agents/executor/openai/responsesexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
	"github.com/openai/openai-go/responses"
)

// OpenAIResponsesAdapter binds one resolved route to a native Responses service.
type OpenAIResponsesAdapter func(context.Context, modelrouter.Plan) (OpenAIResponsesBinding, error)

// OpenAIResponsesBinding contains a typed service and its immutable route plan.
type OpenAIResponsesBinding struct {
	plan           modelrouter.Plan
	service        responses.ResponseService
	resourceLabels map[string]string
	initialized    bool
}

// NewOpenAIResponsesBinding validates plan and constructs a Responses binding.
func NewOpenAIResponsesBinding(plan modelrouter.Plan, service responses.ResponseService, resourceLabels map[string]string) (OpenAIResponsesBinding, error) {
	if err := validateBindingPlan(plan, modelrouter.ProtocolOpenAIResponses); err != nil {
		return OpenAIResponsesBinding{}, err
	}
	service.Options = slices.Clone(service.Options)
	return OpenAIResponsesBinding{plan: plan, service: service, resourceLabels: maps.Clone(resourceLabels), initialized: true}, nil
}

// Plan returns the immutable route plan.
func (b OpenAIResponsesBinding) Plan() modelrouter.Plan { return b.plan }

// Responses returns a copy of the typed service configuration.
func (b OpenAIResponsesBinding) Responses() responses.ResponseService {
	s := b.service
	s.Options = slices.Clone(s.Options)
	return s
}

// ResourceLabels returns a copy of the non-secret provider labels.
func (b OpenAIResponsesBinding) ResourceLabels() map[string]string {
	return maps.Clone(b.resourceLabels)
}

// OpenAIResponsesRegistration registers one provider's Responses adapter.
type OpenAIResponsesRegistration struct {
	Provider modelrouter.Provider
	Adapter  OpenAIResponsesAdapter
}

// OpenAIResponsesAdapterRegistry is an immutable, protocol-fixed adapter registry.
type OpenAIResponsesAdapterRegistry struct {
	adapters map[modelrouter.Provider]OpenAIResponsesAdapter
}

// NewOpenAIResponsesAdapterRegistry validates registrations in declaration order.
func NewOpenAIResponsesAdapterRegistry(registrations ...OpenAIResponsesRegistration) (*OpenAIResponsesAdapterRegistry, error) {
	r := &OpenAIResponsesAdapterRegistry{adapters: make(map[modelrouter.Provider]OpenAIResponsesAdapter, len(registrations))}
	for i, registration := range registrations {
		if err := registration.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("registration %d: %w: %w", i, ErrInvalidAdapter, err)
		}
		if registration.Adapter == nil {
			return nil, fmt.Errorf("registration %d: %w: Responses adapter is nil", i, ErrInvalidAdapter)
		}
		if _, ok := r.adapters[registration.Provider]; ok {
			return nil, fmt.Errorf("registration %d: %w for provider %q", i, ErrDuplicateAdapter, registration.Provider)
		}
		r.adapters[registration.Provider] = registration.Adapter
	}
	return r, nil
}

func (r *OpenAIResponsesAdapterRegistry) lookup(provider modelrouter.Provider) (OpenAIResponsesAdapter, error) {
	if r != nil {
		if adapter, ok := r.adapters[provider]; ok {
			return adapter, nil
		}
	}
	return nil, adapterNotFound(provider, modelrouter.ProtocolOpenAIResponses)
}

func (r RouteResolution) bindOpenAIResponses(ctx context.Context, requirements modelrouter.Requirements) (OpenAIResponsesBinding, error) {
	if err := r.validate(modelrouter.ProtocolOpenAIResponses, requirements); err != nil {
		return OpenAIResponsesBinding{}, err
	}
	adapter, err := r.router.adapters.OpenAIResponses.lookup(r.plan.Provider())
	if err != nil {
		return OpenAIResponsesBinding{}, err
	}
	binding, err := adapter(ctx, r.plan)
	if err != nil {
		return OpenAIResponsesBinding{}, r.adapterError(err)
	}
	if !binding.initialized {
		return OpenAIResponsesBinding{}, fmt.Errorf("%w: adapter returned a zero Responses binding", ErrInvalidBinding)
	}
	if err := validateReturnedPlan(r.plan, binding.Plan()); err != nil {
		return OpenAIResponsesBinding{}, err
	}
	return binding, nil
}

type responsesAgent[Req promptbuilder.Bindable, Resp, CB any] struct {
	executor responsesexecutor.Interface[Req, Resp]
	config   Config[Resp, CB]
}

func newRoutedResponsesAgent[Req promptbuilder.Bindable, Resp, CB any](binding OpenAIResponsesBinding, config Config[Resp, CB]) (Agent[Req, Resp, CB], error) {
	executor, err := responsesexecutor.New[Req](binding.Responses(), responsesexecutor.Config[Resp]{
		Model:               binding.Plan().ProviderModelID(),
		Attribution:         routedAttribution(binding.Plan()),
		UserPrompt:          config.UserPrompt,
		SystemInstructions:  config.SystemInstructions,
		UserPromptSuffix:    config.UserPromptSuffix,
		MaxTurns:            config.MaxTurns,
		MaxTokens:           config.MaxTokens,
		ToolCallConcurrency: config.ToolCallConcurrency,
		Effort:              config.Effort,
		Submit:              submitOptions(config),
		ResultValidators:    config.ResultValidators,
		ResourceLabels:      binding.ResourceLabels(),
		RequestTimeout:      config.ResponsesRequestTimeout,
		ExecutionTimeout:    config.ResponsesExecutionTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &responsesAgent[Req, Resp, CB]{executor: executor, config: config}, nil
}

func (a *responsesAgent[Req, Resp, CB]) Execute(ctx context.Context, request Req, callbacks CB) (Resp, error) {
	tools, err := a.config.Tools.Tools(ctx, callbacks)
	if err != nil {
		var zero Resp
		return zero, fmt.Errorf("building Responses tools: %w", err)
	}
	return a.executor.Execute(ctx, request, tools)
}
