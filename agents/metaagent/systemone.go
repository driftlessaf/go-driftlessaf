/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"fmt"
	"maps"

	"chainguard.dev/driftlessaf/agents/executor/systemone"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// SystemOneService asks typed questions against one state. A provider adapter
// may implement it with the System One HTTP client or its own serving API.
type SystemOneService interface {
	Ask(context.Context, systemone.Request) (*systemone.Response, error)
}

// SystemOneAdapter binds a resolved route to a provider's serving account.
type SystemOneAdapter func(context.Context, modelrouter.Plan) (SystemOneBinding, error)

// SystemOneBinding carries the selected route and its typed request service.
type SystemOneBinding struct {
	plan           modelrouter.Plan
	service        SystemOneService
	resourceLabels map[string]string
	initialized    bool
}

// NewSystemOneBinding validates the route and service returned by an adapter.
// Its service pins requests to the route's provider model ID.
func NewSystemOneBinding(plan modelrouter.Plan, service SystemOneService, resourceLabels map[string]string) (SystemOneBinding, error) {
	if err := validateBindingPlan(plan, modelrouter.ProtocolSystemOne); err != nil {
		return SystemOneBinding{}, err
	}
	if service == nil {
		return SystemOneBinding{}, fmt.Errorf("%w: System One service is nil", ErrInvalidBinding)
	}
	return SystemOneBinding{plan: plan, service: routedSystemOneService{service: service, model: plan.ProviderModelID()}, resourceLabels: maps.Clone(resourceLabels), initialized: true}, nil
}

type routedSystemOneService struct {
	service SystemOneService
	model   string
}

func (s routedSystemOneService) Ask(ctx context.Context, request systemone.Request) (*systemone.Response, error) {
	if request.Model != "" && request.Model != s.model {
		return nil, fmt.Errorf("%w: request model %q differs from routed model %q", systemone.ErrInvalidRequest, request.Model, s.model)
	}
	request.Model = s.model
	return s.service.Ask(ctx, request)
}

// Plan returns the selected, secret-free route.
func (b SystemOneBinding) Plan() modelrouter.Plan { return b.plan }

// Service returns the typed question service.
func (b SystemOneBinding) Service() SystemOneService { return b.service }

// ResourceLabels returns a copy of the provider resource labels.
func (b SystemOneBinding) ResourceLabels() map[string]string { return maps.Clone(b.resourceLabels) }

// SystemOneRegistration registers a provider's System One adapter.
type SystemOneRegistration struct {
	Provider modelrouter.Provider
	Adapter  SystemOneAdapter
}

// SystemOneAdapterRegistry holds one adapter per serving provider.
type SystemOneAdapterRegistry struct {
	adapters map[modelrouter.Provider]SystemOneAdapter
}

// NewSystemOneAdapterRegistry validates registrations in declaration order.
func NewSystemOneAdapterRegistry(registrations ...SystemOneRegistration) (*SystemOneAdapterRegistry, error) {
	registry := &SystemOneAdapterRegistry{adapters: make(map[modelrouter.Provider]SystemOneAdapter, len(registrations))}
	firstIndex := make(map[modelrouter.Provider]int, len(registrations))
	for i, registration := range registrations {
		if err := registration.Provider.Validate(); err != nil {
			return nil, fmt.Errorf("registration %d: %w: provider: %w", i, ErrInvalidAdapter, err)
		}
		if registration.Adapter == nil {
			return nil, fmt.Errorf("registration %d: %w: System One adapter is nil", i, ErrInvalidAdapter)
		}
		if first, ok := firstIndex[registration.Provider]; ok {
			return nil, fmt.Errorf("registration %d: %w for provider %q and protocol %q (first registered at registration %d)",
				i, ErrDuplicateAdapter, registration.Provider, modelrouter.ProtocolSystemOne, first)
		}
		firstIndex[registration.Provider] = i
		registry.adapters[registration.Provider] = registration.Adapter
	}
	return registry, nil
}

func (r *SystemOneAdapterRegistry) lookup(provider modelrouter.Provider) (SystemOneAdapter, error) {
	if r != nil {
		if adapter, ok := r.adapters[provider]; ok {
			return adapter, nil
		}
	}
	return nil, adapterNotFound(provider, modelrouter.ProtocolSystemOne)
}
