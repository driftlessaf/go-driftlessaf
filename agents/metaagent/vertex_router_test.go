/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func vertexRouterTestRoute(protocol modelrouter.Protocol, id string) modelrouter.Route {
	route := routedTestRoute(modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: id}, protocol, id)
	route.Attribution = modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: "google.vertex"}
	return route
}

func TestNewVertexRouterConstructsEachDeclaredProtocol(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
	// Explicit routing must not inherit the compatibility constructor's backend.
	t.Setenv("CLAUDE_BACKEND", "unsupported")
	t.Setenv("ANTHROPIC_PROFILE", "missing-profile")
	routes := []modelrouter.Route{
		vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash"),
		vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-pro"),
		vertexRouterTestRoute(modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6"),
		vertexRouterTestRoute(modelrouter.ProtocolOpenAIChatCompletions, "google/gemini-2.5-flash"),
	}
	router, err := NewVertexRouter("test-project", "global", routes...)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		t.Run(route.Selection.LogicalModel, func(t *testing.T) {
			agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, routedTestConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			if agent == nil {
				t.Fatal("NewRouted returned a nil agent")
			}
			resolution, err := router.Resolve(route.Selection)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolution.Plan().ProviderModelID(); got != route.ProviderModelID {
				t.Errorf("provider model: got = %q, want = %q", got, route.ProviderModelID)
			}
		})
	}
}

func TestNewVertexRouterValidatesWithoutCredentials(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/missing/credentials.json")
	valid := vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")
	foreign := vertexRouterTestRoute(modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")
	foreign.Selection.Provider = modelrouter.ProviderAnthropic
	wrongAttribution := valid
	wrongAttribution.Attribution = modelrouter.Attribution{ProviderName: "anthropic", LegacySystem: "anthropic"}
	for _, tc := range []struct {
		name            string
		project, region string
		routes          []modelrouter.Route
		wantErr         error
	}{
		{name: "valid lazy router", project: "test-project", region: "global", routes: []modelrouter.Route{valid}},
		{name: "empty routes", project: "test-project", region: "global", wantErr: ErrInvalidRouter},
		{name: "invalid project", project: "test/project", region: "global", routes: []modelrouter.Route{valid}, wantErr: ErrInvalidAdapter},
		{name: "invalid region", project: "test-project", region: "", routes: []modelrouter.Route{valid}, wantErr: ErrInvalidAdapter},
		{name: "other provider", project: "test-project", region: "global", routes: []modelrouter.Route{foreign}, wantErr: ErrInvalidBinding},
		{name: "wrong attribution", project: "test-project", region: "global", routes: []modelrouter.Route{wrongAttribution}, wantErr: ErrInvalidBinding},
		{name: "duplicate selection", project: "test-project", region: "global", routes: []modelrouter.Route{valid, valid}, wantErr: modelrouter.ErrDuplicateRoute},
		{name: "unsupported protocol", project: "test-project", region: "global", routes: []modelrouter.Route{vertexRouterTestRoute(modelrouter.ProtocolOpenAIResponses, "example/model")}, wantErr: ErrInvalidRouter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, err := NewVertexRouter(tc.project, tc.region, tc.routes...)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewVertexRouter: got = %v, want = %v", err, tc.wantErr)
			}
			if err == nil {
				if _, err := router.adapters.AnthropicMessages.lookup(modelrouter.ProviderVertexAI); !errors.Is(err, ErrAdapterNotFound) {
					t.Errorf("undeclared protocol adapter: got = %v, want = ErrAdapterNotFound", err)
				}
				if _, err := NewRouted[*testRequest](t.Context(), router, valid.Selection, routedTestConfig(t)); err == nil {
					t.Fatal("binding succeeded with missing credentials")
				}
			}
		})
	}
}

func TestNewVertexRouterCopiesDeclarations(t *testing.T) {
	t.Parallel()
	route := vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")
	selection := route.Selection
	router, err := NewVertexRouter("test-project", "global", route)
	if err != nil {
		t.Fatal(err)
	}
	route.ProviderModelID = "changed-after-construction"
	route.Capabilities.ToolCalling = false
	resolution, err := router.Resolve(selection)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Plan().ProviderModelID() != selection.LogicalModel || !resolution.Plan().Capabilities().ToolCalling {
		t.Fatal("caller mutation changed the router's plan")
	}
	missing := selection
	missing.LogicalModel = "gemini-undeclared"
	if _, err := router.Resolve(missing); !errors.Is(err, modelrouter.ErrRouteNotFound) {
		t.Errorf("undeclared route: got = %v, want = ErrRouteNotFound", err)
	}
}
