/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"fmt"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

// Backend is an immutable registration for one provider account. Create
// it with VertexBackend, BedrockBackend, AnthropicBackend, or NewAdapterBackend.
// Its zero value is invalid. Names default to the provider name; distinct names
// isolate accounts. Factories capture configuration, never route selection.
type Backend struct {
	name     string
	provider modelrouter.Provider
	region   func(string) string
	build    func(string, []modelrouter.Route) (*Router, error)
}

// VertexBackend captures one Vertex project. Every target must supply a region.
// Construction and target resolution do not discover credentials.
func VertexBackend(name string, cfg VertexConfig) Backend {
	return Backend{
		name: name, provider: modelrouter.ProviderVertexAI,
		region: func(region string) string { return region },
		build: func(region string, routes []modelrouter.Route) (*Router, error) {
			return NewVertexRouter(cfg.ProjectID, region, routes...)
		},
	}
}

// BedrockBackend captures one AWS credential configuration and default region.
// A target's region overrides cfg.Region. Credentials remain lazy.
func BedrockBackend(name string, cfg awsauth.Config) Backend {
	return Backend{
		name: name, provider: modelrouter.ProviderAWSBedrock,
		region: func(region string) string { return cmp.Or(region, cfg.Region) },
		build: func(region string, routes []modelrouter.Route) (*Router, error) {
			selected := cfg
			selected.Region = region
			return NewBedrockRuntimeRouter(selected, routes...)
		},
	}
}

// AnthropicBackend captures direct Anthropic authentication configuration.
// Target regions are ignored, and credentials are discovered only on binding.
func AnthropicBackend(name string, cfg anthropicauth.Config) Backend {
	backend := NewAdapterBackend(name, modelrouter.ProviderAnthropic, func(string) (AdapterRegistrations, error) {
		adapter, err := NewAnthropicDirectMessagesAdapter(cfg)
		if err != nil {
			return AdapterRegistrations{}, err
		}
		return AdapterRegistrations{AnthropicMessages: []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderAnthropic, Adapter: adapter}}}, nil
	})
	backend.region = func(string) string { return "" }
	return backend
}

// NewAdapterBackend captures a factory for typed provider adapters. It does not
// invoke the factory until a declared target is resolved. The target region is
// passed unchanged and forms part of the router cache key, including when empty.
//
// The factory must return configuration-only adapters without loading credentials
// or making requests; credentials belong in adapter binding after validation.
// It must capture immutable configuration and return only the specified provider's
// registrations. Adapters must be safe for concurrent binding. Failed factory
// calls are not cached. Each Runtime serializes factory calls; a registration
// reused across runtimes must also support concurrent factory calls.
func NewAdapterBackend(name string, provider modelrouter.Provider, factory func(string) (AdapterRegistrations, error)) Backend {
	backend := Backend{name: name, provider: provider, region: func(region string) string { return region }}
	if factory == nil {
		return backend
	}
	backend.build = func(region string, routes []modelrouter.Route) (*Router, error) {
		registrations, err := factory(region)
		if err != nil {
			return nil, err
		}
		// Reject cross-provider registrations before constructing a router. A named
		// backend must not supply another provider's credentials.
		for _, r := range registrations.GoogleGenAI {
			if r.Provider != provider {
				return nil, fmt.Errorf("%w: backend provider mismatch", ErrInvalidRouter)
			}
		}
		for _, r := range registrations.AnthropicMessages {
			if r.Provider != provider {
				return nil, fmt.Errorf("%w: backend provider mismatch", ErrInvalidRouter)
			}
		}
		for _, r := range registrations.OpenAIChatCompletions {
			if r.Provider != provider {
				return nil, fmt.Errorf("%w: backend provider mismatch", ErrInvalidRouter)
			}
		}
		for _, r := range registrations.OpenAIResponses {
			if r.Provider != provider {
				return nil, fmt.Errorf("%w: backend provider mismatch", ErrInvalidRouter)
			}
		}
		registry, err := modelrouter.NewRegistry(routes...)
		if err != nil {
			return nil, err
		}
		return NewRouterWithAdapters(registry, registrations)
	}
	return backend
}
