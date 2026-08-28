/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"chainguard.dev/driftlessaf/agents/model"
)

var (
	// ErrInvalidRoute identifies an invalid route declaration.
	ErrInvalidRoute = errors.New("invalid route")
	// ErrInvalidSelection identifies an invalid route selection.
	ErrInvalidSelection = errors.New("invalid route selection")
	// ErrDuplicateRoute identifies two declarations for the same selection.
	ErrDuplicateRoute = errors.New("duplicate route")
	// ErrRouteNotFound identifies a valid selection that has no declaration.
	ErrRouteNotFound = errors.New("route not found")
	// ErrUnsupportedCapability identifies requirements that a route cannot
	// satisfy.
	ErrUnsupportedCapability = errors.New("unsupported route capability")
	// ErrInvalidPlan identifies a zero or otherwise unresolved plan.
	ErrInvalidPlan = errors.New("invalid route plan")
)

// Selection identifies one provider and logical model. It deliberately
// contains neither protocol nor provider model ID: those values come from the
// application's validated route catalog.
type Selection struct {
	Provider     Provider
	LogicalModel string
}

// Validate verifies that s can be used as a route lookup key.
func (s Selection) Validate() error {
	if err := s.Provider.Validate(); err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if err := validateModelID("logical model", s.LogicalModel); err != nil {
		return err
	}
	return nil
}

// Route declares how one selection runs and the exact capabilities that route
// allows. The effective plan intersects this allow-set with model-family and
// protocol support. Route values are copied by NewRegistry and remain safe for
// the caller to reuse or mutate.
type Route struct {
	Selection       Selection
	Protocol        Protocol
	ProviderModelID string
	Capabilities    Capabilities
}

// Registry is an immutable collection of validated routes. Construct one
// explicitly with NewRegistry; the package has no global registry state.
type Registry struct {
	routes map[Selection]Plan
}

// Plan is the immutable, validated, and secret-free result of resolving a
// route. Its private fields can be inspected only through copy-returning
// accessors.
type Plan struct {
	selection       Selection
	protocol        Protocol
	providerModelID string
	capabilities    Capabilities
}

// NewRegistry validates routes and returns an immutable registry. Validation
// follows declaration order so errors are deterministic.
func NewRegistry(routes ...Route) (*Registry, error) {
	registry := &Registry{routes: make(map[Selection]Plan, len(routes))}
	firstIndex := make(map[Selection]int, len(routes))
	for i, route := range routes {
		plan, err := planRoute(route)
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", i, err)
		}
		if first, ok := firstIndex[route.Selection]; ok {
			return nil, fmt.Errorf("route %d: %w for provider %q and logical model %q (first declared at route %d)",
				i, ErrDuplicateRoute, route.Selection.Provider, route.Selection.LogicalModel, first)
		}
		firstIndex[route.Selection] = i
		registry.routes[route.Selection] = plan
	}
	return registry, nil
}

// Resolve returns the plan declared for selection. It doesn't construct a
// client or load credentials.
func (r *Registry) Resolve(selection Selection) (Plan, error) {
	if err := selection.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalidSelection, err)
	}
	if r == nil {
		return Plan{}, fmt.Errorf("%w: registry is nil", ErrRouteNotFound)
	}
	plan, ok := r.routes[selection]
	if !ok {
		return Plan{}, fmt.Errorf("%w for provider %q and logical model %q", ErrRouteNotFound, selection.Provider, selection.LogicalModel)
	}
	return plan, nil
}

// Selection returns the provider and logical model used to resolve p.
func (p Plan) Selection() Selection {
	return p.selection
}

// Provider returns the serving provider.
func (p Plan) Provider() Provider {
	return p.selection.Provider
}

// LogicalModel returns the application-selected logical model ID.
func (p Plan) LogicalModel() string {
	return p.selection.LogicalModel
}

// Protocol returns the executor wire protocol.
func (p Plan) Protocol() Protocol {
	return p.protocol
}

// ProviderModelID returns the exact model ID to send to the provider.
func (p Plan) ProviderModelID() string {
	return p.providerModelID
}

// Capabilities returns a copy of the route's effective capabilities.
func (p Plan) Capabilities() Capabilities {
	return cloneCapabilities(p.capabilities)
}

// Validate verifies that p was produced by successful route resolution.
func (p Plan) Validate() error {
	if err := p.selection.Validate(); err != nil {
		return fmt.Errorf("%w: selection: %w", ErrInvalidPlan, err)
	}
	if err := p.protocol.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPlan, err)
	}
	if err := validateModelID("provider model ID", p.providerModelID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPlan, err)
	}
	if err := validateCapabilities(p.capabilities); err != nil {
		return fmt.Errorf("%w: capabilities: %w", ErrInvalidPlan, err)
	}
	return nil
}

// ValidateRequirements verifies that p supports every requested feature. A
// caller can run this before invoking an adapter or constructing a provider
// client.
func (p Plan) ValidateRequirements(requirements Requirements) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := validateRequirements(p.capabilities, requirements); err != nil {
		return fmt.Errorf("route for provider %q and logical model %q: %w", p.Provider(), p.LogicalModel(), err)
	}
	return nil
}

func planRoute(route Route) (Plan, error) {
	if err := route.Selection.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: selection: %w", ErrInvalidRoute, err)
	}
	if err := route.Protocol.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalidRoute, err)
	}
	if err := validateModelID("provider model ID", route.ProviderModelID); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalidRoute, err)
	}
	if err := validateCapabilities(route.Capabilities); err != nil {
		return Plan{}, fmt.Errorf("%w: capabilities: %w", ErrInvalidRoute, err)
	}

	info := model.Resolve(route.Selection.LogicalModel)
	if info.Backend == model.BackendUnknown {
		return Plan{}, fmt.Errorf("%w: logical model %q isn't recognized by agents/model", ErrInvalidRoute, route.Selection.LogicalModel)
	}
	wantProtocol := protocolForBackend(info.Backend)
	if route.Protocol != wantProtocol {
		return Plan{}, fmt.Errorf("%w: logical model %q resolves to backend %q, which requires protocol %q, not %q",
			ErrInvalidRoute, route.Selection.LogicalModel, info.Backend, wantProtocol, route.Protocol)
	}

	declaredCapabilities := cloneCapabilities(route.Capabilities)
	return Plan{
		selection:       route.Selection,
		protocol:        route.Protocol,
		providerModelID: route.ProviderModelID,
		capabilities:    composeCapabilities(route.Protocol, info, declaredCapabilities),
	}, nil
}

func protocolForBackend(backend model.Backend) Protocol {
	switch backend {
	case model.BackendGemini:
		return ProtocolGoogleGenAI
	case model.BackendClaude:
		return ProtocolAnthropicMessages
	case model.BackendOpenAICompat:
		return ProtocolOpenAIChatCompletions
	default:
		return ""
	}
}

func validateModelID(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s %q must not have leading or trailing whitespace", name, value)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s %q must not contain control characters", name, value)
		}
	}
	return nil
}
