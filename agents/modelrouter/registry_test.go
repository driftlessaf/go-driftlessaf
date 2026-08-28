/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/google/go-cmp/cmp"
)

func TestRegistryResolvesDeclaredRoutes(t *testing.T) {
	t.Parallel()

	routes := []modelrouter.Route{
		newRoute(modelrouter.ProviderVertexAI, "gemini-3-flash-preview", modelrouter.ProtocolGoogleGenAI, "gemini-3-flash-preview"),
		newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5@20260301"),
		newRoute(modelrouter.ProviderVertexAI, "meta/llama-3.3", modelrouter.ProtocolOpenAIChatCompletions, "meta/llama-3.3-70b-instruct-maas"),
		newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5-20260301"),
		newRoute(modelrouter.ProviderAWSBedrock, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "anthropic.claude-sonnet-5"),
	}
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	for _, route := range routes {
		t.Run(fmt.Sprintf("%s/%s", route.Selection.Provider, route.Selection.LogicalModel), func(t *testing.T) {
			t.Parallel()
			plan, err := registry.Resolve(route.Selection)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := plan.Selection(); got != route.Selection {
				t.Errorf("Selection() = %+v, want %+v", got, route.Selection)
			}
			if got := plan.Provider(); got != route.Selection.Provider {
				t.Errorf("Provider() = %q, want %q", got, route.Selection.Provider)
			}
			if got := plan.LogicalModel(); got != route.Selection.LogicalModel {
				t.Errorf("LogicalModel() = %q, want %q", got, route.Selection.LogicalModel)
			}
			if got := plan.Protocol(); got != route.Protocol {
				t.Errorf("Protocol() = %q, want %q", got, route.Protocol)
			}
			if got := plan.ProviderModelID(); got != route.ProviderModelID {
				t.Errorf("ProviderModelID() = %q, want %q", got, route.ProviderModelID)
			}
		})
	}
}

func TestRegistryAcceptsExtensionProvider(t *testing.T) {
	t.Parallel()

	const provider modelrouter.Provider = "test-provider"
	selection := modelrouter.Selection{Provider: provider, LogicalModel: "claude-sonnet-5"}
	registry, err := modelrouter.NewRegistry(modelrouter.Route{
		Selection:       selection,
		Protocol:        modelrouter.ProtocolAnthropicMessages,
		ProviderModelID: "test/sonnet-5",
	})
	if err != nil {
		t.Fatalf("NewRegistry() with an extension provider error = %v", err)
	}
	plan, err := registry.Resolve(selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := plan.Provider(); got != provider {
		t.Errorf("Provider() = %q, want %q", got, provider)
	}
}

func TestNewRegistryRejectsInvalidRoutesDeterministically(t *testing.T) {
	t.Parallel()

	valid := newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
	tests := []struct {
		name    string
		route   modelrouter.Route
		wantErr error
		want    string
	}{
		{
			name:    "empty provider",
			route:   newRoute("", "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    "route 0: invalid route: selection: provider: provider must not be empty",
		},
		{
			name:    "invalid provider",
			route:   newRoute("Vertex", "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: selection: provider: invalid provider "Vertex": must be lowercase ASCII letters or digits separated by '.', '_', or '-'`,
		},
		{
			name:    "empty logical model",
			route:   newRoute(modelrouter.ProviderVertexAI, "", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    "route 0: invalid route: selection: logical model must not be empty",
		},
		{
			name:    "logical model whitespace",
			route:   newRoute(modelrouter.ProviderVertexAI, " claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: selection: logical model " claude-sonnet-5" must not have leading or trailing whitespace`,
		},
		{
			name:    "unsupported protocol",
			route:   newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", "anthropic-responses", "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: unsupported protocol "anthropic-responses" (want "google-gen-ai", "anthropic-messages", or "openai-chat-completions")`,
		},
		{
			name:    "empty provider model ID",
			route:   newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, ""),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    "route 0: invalid route: provider model ID must not be empty",
		},
		{
			name:    "provider model ID control character",
			route:   newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude\nsonnet"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    "route 0: invalid route: provider model ID \"claude\\nsonnet\" must not contain control characters",
		},
		{
			name:    "provider model ID Unicode control character",
			route:   newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude\u0085sonnet"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    "route 0: invalid route: provider model ID \"claude\\u0085sonnet\" must not contain control characters",
		},
		{
			name: "invalid declared effort",
			route: func() modelrouter.Route {
				route := valid
				route.Capabilities.Efforts = []effort.Level{"turbo"}
				return route
			}(),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: capabilities: effort at index 0: invalid effort level "turbo" (want low|medium|high|xhigh|max)`,
		},
		{
			name: "duplicate declared effort",
			route: func() modelrouter.Route {
				route := valid
				route.Capabilities.Efforts = []effort.Level{effort.High, effort.High}
				return route
			}(),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: capabilities: duplicate effort "high"`,
		},
		{
			name:    "unknown logical model",
			route:   newRoute(modelrouter.ProviderVertexAI, "gpt-4o", modelrouter.ProtocolOpenAIChatCompletions, "gpt-4o"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: logical model "gpt-4o" isn't recognized by agents/model`,
		},
		{
			name:    "protocol doesn't match model",
			route:   newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolGoogleGenAI, "claude-sonnet-5"),
			wantErr: modelrouter.ErrInvalidRoute,
			want:    `route 0: invalid route: logical model "claude-sonnet-5" resolves to backend "claude", which requires protocol "anthropic-messages", not "google-gen-ai"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := modelrouter.NewRegistry(tt.route)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewRegistry() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("NewRegistry() error mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestNewRegistryRejectsDuplicateSelection(t *testing.T) {
	t.Parallel()

	route := newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
	duplicate := route
	duplicate.ProviderModelID = "claude-sonnet-5@different"
	_, err := modelrouter.NewRegistry(route, duplicate)
	if !errors.Is(err, modelrouter.ErrDuplicateRoute) {
		t.Fatalf("NewRegistry() error = %v, want ErrDuplicateRoute", err)
	}
	want := `route 1: duplicate route for provider "vertex" and logical model "claude-sonnet-5" (first declared at route 0)`
	if got := err.Error(); got != want {
		t.Errorf("NewRegistry() error = %q, want %q", got, want)
	}
}

func TestResolveErrors(t *testing.T) {
	t.Parallel()

	registry, err := modelrouter.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	tests := []struct {
		name    string
		resolve func() error
		wantErr error
		want    string
	}{
		{
			name: "invalid selection",
			resolve: func() error {
				_, err := registry.Resolve(modelrouter.Selection{})
				return err
			},
			wantErr: modelrouter.ErrInvalidSelection,
			want:    "invalid route selection: provider: provider must not be empty",
		},
		{
			name: "missing route",
			resolve: func() error {
				_, err := registry.Resolve(modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "claude-sonnet-5"})
				return err
			},
			wantErr: modelrouter.ErrRouteNotFound,
			want:    `route not found for provider "vertex" and logical model "claude-sonnet-5"`,
		},
		{
			name: "nil registry",
			resolve: func() error {
				var nilRegistry *modelrouter.Registry
				_, err := nilRegistry.Resolve(modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "claude-sonnet-5"})
				return err
			},
			wantErr: modelrouter.ErrRouteNotFound,
			want:    "route not found: registry is nil",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.resolve()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Resolve() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("Resolve() error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanValidate(t *testing.T) {
	t.Parallel()

	route := newRoute(modelrouter.ProviderVertexAI, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := plan.Validate(); err != nil {
		t.Errorf("Validate() resolved plan error = %v", err)
	}

	var zero modelrouter.Plan
	if err := zero.Validate(); !errors.Is(err, modelrouter.ErrInvalidPlan) {
		t.Errorf("zero Plan.Validate() error = %v, want ErrInvalidPlan", err)
	}
	if err := zero.ValidateRequirements(modelrouter.Requirements{}); !errors.Is(err, modelrouter.ErrInvalidPlan) {
		t.Errorf("zero Plan.ValidateRequirements() error = %v, want ErrInvalidPlan", err)
	}
}

func TestPlanDoesNotExposeCredentialFields(t *testing.T) {
	t.Parallel()

	for _, value := range []any{
		modelrouter.Selection{},
		modelrouter.Route{},
		modelrouter.Plan{},
		modelrouter.Capabilities{},
	} {
		typeOf := reflect.TypeOf(value)
		for field := range typeOf.Fields() {
			name := strings.ToLower(field.Name)
			for _, forbidden := range []string{"secret", "credential", "tokensource", "apikey", "client", "endpoint", "authconfig"} {
				if strings.Contains(name, forbidden) {
					t.Errorf("%s contains credential-bearing field %q", typeOf, field.Name)
				}
			}
		}
	}
}

func TestRegistryCopiesRouteInputAndPlanAccessors(t *testing.T) {
	t.Parallel()

	allowed := []effort.Level{effort.Low, effort.High}
	route := newRoute(modelrouter.ProviderAWSBedrock, "claude-sonnet-4-6", modelrouter.ProtocolAnthropicMessages, "anthropic.claude-sonnet-4-6")
	route.Capabilities.Efforts = allowed
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	allowed[0] = effort.Max
	route.Capabilities.Efforts[1] = effort.Max
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := []effort.Level{effort.Low, effort.High}
	if diff := cmp.Diff(want, plan.Capabilities().Efforts); diff != "" {
		t.Fatalf("Capabilities().Efforts after input mutation mismatch (-want, +got):\n%s", diff)
	}

	first := plan.Capabilities()
	first.Efforts[0] = effort.Max
	first.Efforts = append(first.Efforts, effort.Medium)
	if diff := cmp.Diff(want, plan.Capabilities().Efforts); diff != "" {
		t.Errorf("Capabilities().Efforts after accessor mutation mismatch (-want, +got):\n%s", diff)
	}
}

func newRoute(provider modelrouter.Provider, logicalModel string, protocol modelrouter.Protocol, providerModelID string) modelrouter.Route {
	return modelrouter.Route{
		Selection: modelrouter.Selection{
			Provider:     provider,
			LogicalModel: logicalModel,
		},
		Protocol:        protocol,
		ProviderModelID: providerModelID,
		Capabilities: modelrouter.Capabilities{
			Efforts:                []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
			ExplicitThinkingBudget: true,
			SamplingParameters:     true,
			PromptCaching:          true,
			ToolCalling:            true,
			TerminalSubmission:     true,
			SuspendResume:          true,
			MaximumOutputTokens:    true,
			RefusalRecovery:        true,
		},
	}
}
