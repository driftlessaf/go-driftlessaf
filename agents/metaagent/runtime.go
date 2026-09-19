/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"cmp"
	"context"
	"fmt"
	"sync"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/promptbuilder"
)

// VertexConfig configures the Google Cloud project used by Vertex AI.
// Authentication uses Application Default Credentials; regions belong to targets.
type VertexConfig struct {
	// ProjectID is required when resolving a Vertex AI target.
	ProjectID string
}

// Backend is one explicitly configured provider account. Name defaults to the
// provider name. Use distinct names for separate projects or auth configurations.
// Credentials configure the selected backend; they never select a provider.
type Backend struct {
	Name     string
	Provider modelrouter.Provider
	// Google configures Vertex AI.
	Google VertexConfig
	// Anthropic holds typed WIF configuration for Anthropic-direct.
	Anthropic anthropicauth.Config
	// AWS configures Bedrock Runtime. A target region overrides AWS.Region.
	// AWS.Profile selects SSO; otherwise the existing web-identity credential
	// chain is resolved when the agent binds its adapter.
	AWS awsauth.Config
}

// TargetConfig selects an exact provider/model pair on a configured backend.
// It contains no credentials and can be populated from application configuration.
// Backend defaults to the provider name. Region is required for Vertex AI and
// ignored for Anthropic-direct. For Bedrock it overrides Backend.AWS.Region;
// one of those regions must be set. Catalog probe regions are never defaults.
type TargetConfig struct {
	Provider modelrouter.Provider `env:"PROVIDER"`
	Model    string               `env:"MODEL"`
	Region   string               `env:"REGION"`
	Backend  string               `env:"BACKEND"`
}

// Runtime owns a route catalog and reusable routers for one application startup.
// It is safe for concurrent use. It caches configuration, not agents, callbacks,
// SDK clients, or failed construction attempts. There is no global cache.
type Runtime struct {
	routes       *modelrouter.Registry
	declarations map[modelrouter.Provider][]modelrouter.Route
	backends     map[string]Backend
	mu           sync.Mutex
	routers      map[runtimeKey]*Router
}

type runtimeKey struct {
	backend string
	region  string
}

// Target is an immutable selection associated with a startup Runtime. Copying
// it is safe; the zero value returns ErrInvalidRouter when resolved. Target
// creation does not validate configuration or discover credentials.
type Target struct {
	runtime *Runtime
	config  TargetConfig
}

// NewRuntime snapshots the catalog and backend configurations without loading
// credentials or reading environment variables. Vertex AI, Anthropic-direct,
// and Bedrock Runtime backends are supported; custom adapters can continue using
// NewRouterWithAdapters. Duplicate backend names and invalid catalogs fail here;
// selected backend settings are validated when a target is resolved.
func NewRuntime(routes []modelrouter.Route, backends ...Backend) (*Runtime, error) {
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		routes:       registry,
		declarations: make(map[modelrouter.Provider][]modelrouter.Route),
		backends:     make(map[string]Backend, len(backends)),
		routers:      make(map[runtimeKey]*Router),
	}
	for _, route := range routes {
		// The registry owns copies, including mutable capability slices.
		plan, err := registry.Resolve(route.Selection)
		if err != nil {
			return nil, err
		}
		route.Capabilities = plan.Capabilities()
		r.declarations[route.Selection.Provider] = append(r.declarations[route.Selection.Provider], route)
	}
	for _, backend := range backends {
		if backend.Provider != modelrouter.ProviderVertexAI && backend.Provider != modelrouter.ProviderAnthropic && backend.Provider != modelrouter.ProviderAWSBedrock {
			return nil, fmt.Errorf("%w: unsupported runtime provider %q", ErrInvalidRouter, backend.Provider)
		}
		backend.Name = cmp.Or(backend.Name, string(backend.Provider))
		if _, ok := r.backends[backend.Name]; ok {
			return nil, fmt.Errorf("%w: duplicate backend %q", ErrInvalidRouter, backend.Name)
		}
		r.backends[backend.Name] = backend
	}
	return r, nil
}

// Target describes a role's provider, model, region, and backend. Validation is
// deferred to Resolve or NewWithTarget so callers use normal constructor errors.
func (r *Runtime) Target(config TargetConfig) Target {
	return Target{runtime: r, config: config}
}

// Resolve returns the shared router and exact selection without binding an
// adapter or loading credentials. Routers are shared by backend and serving
// region, independent of the model. The caller must still validate its own
// feature requirements before binding, as NewRouted and judge.NewRouted do.
func (t Target) Resolve() (*Router, modelrouter.Selection, error) {
	selection := modelrouter.Selection{Provider: t.config.Provider, LogicalModel: t.config.Model}
	if t.runtime == nil || t.runtime.routes == nil {
		return nil, selection, fmt.Errorf("%w: target has no runtime", ErrInvalidRouter)
	}
	router, err := t.runtime.resolve(t.config, selection)
	if err != nil {
		return nil, selection, fmt.Errorf("target provider %q model %q: %w", selection.Provider, selection.LogicalModel, err)
	}
	return router, selection, nil
}

func (r *Runtime) resolve(config TargetConfig, selection modelrouter.Selection) (*Router, error) {
	if _, err := r.routes.Resolve(selection); err != nil {
		return nil, err
	}
	key := runtimeKey{backend: cmp.Or(config.Backend, string(config.Provider)), region: config.Region}
	backend, ok := r.backends[key.backend]
	if !ok || backend.Provider != config.Provider {
		return nil, fmt.Errorf("%w: backend %q is not configured for provider %q", ErrInvalidRouter, key.backend, config.Provider)
	}
	if config.Provider == modelrouter.ProviderAWSBedrock {
		key.region = cmp.Or(config.Region, backend.AWS.Region)
	}
	if config.Provider == modelrouter.ProviderAnthropic {
		key.region = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if router, ok := r.routers[key]; ok {
		return router, nil
	}
	var router *Router
	var err error
	switch backend.Provider {
	case modelrouter.ProviderVertexAI:
		router, err = NewVertexRouter(backend.Google.ProjectID, key.region, r.declarations[backend.Provider]...)
	case modelrouter.ProviderAWSBedrock:
		cfg := backend.AWS
		cfg.Region = key.region
		router, err = NewBedrockRuntimeRouter(cfg, r.declarations[backend.Provider]...)
	case modelrouter.ProviderAnthropic:
		var adapter AnthropicMessagesAdapter
		adapter, err = NewAnthropicDirectMessagesAdapter(backend.Anthropic)
		if err == nil {
			router, err = NewRouterWithAdapters(r.routes, AdapterRegistrations{
				AnthropicMessages: []AnthropicMessagesRegistration{{Provider: backend.Provider, Adapter: adapter}},
			})
		}
	}
	if err != nil {
		return nil, err
	}
	r.routers[key] = router
	return router, nil
}

// NewWithTarget resolves a startup-owned target and delegates to NewRouted.
// It preserves configuration, capability, schema, and adapter validation and
// never falls back to a different provider or the legacy constructor.
func NewWithTarget[Req promptbuilder.Bindable, Resp, CB any](ctx context.Context, target Target, config Config[Resp, CB]) (Agent[Req, Resp, CB], error) {
	router, selection, err := target.Resolve()
	if err != nil {
		return nil, err
	}
	return NewRouted[Req](ctx, router, selection, config)
}
