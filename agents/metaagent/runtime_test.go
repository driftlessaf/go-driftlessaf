/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"chainguard.dev/driftlessaf/agents/anthropicauth"
	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func runtimeRoutes() []modelrouter.Route {
	claude := vertexRouterTestRoute(modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")
	direct := claude
	direct.Selection.Provider = modelrouter.ProviderAnthropic
	direct.Attribution = modelrouter.Attribution{ProviderName: "anthropic", LegacySystem: "anthropic"}
	return []modelrouter.Route{claude, direct, vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash")}
}

func TestRuntimeSharesOnlyMatchingBackends(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/missing/credentials.json")
	t.Setenv("ANTHROPIC_PROFILE", "must-not-load")
	t.Setenv("CLAUDE_BACKEND", "must-not-select")
	runtime, err := NewRuntime(runtimeRoutes(),
		Backend{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project-one"}},
		Backend{Name: "other-project", Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project-two"}},
		Backend{Provider: modelrouter.ProviderAnthropic, Anthropic: anthropicauth.Config{FederationRuleID: "rule", OrganizationID: "org"}},
		Backend{Name: "other-account", Provider: modelrouter.ProviderAnthropic, Anthropic: anthropicauth.Config{FederationRuleID: "rule-two", OrganizationID: "org"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var routers []*Router
	for _, tc := range []struct {
		name       string
		config     TargetConfig
		sharedWith int
	}{
		{"reviewer", TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "global"}, -1},
		{"same model fixer", TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "global"}, 0},
		{"different model", TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "gemini-2.5-flash", Region: "global"}, 0},
		{"different region", TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "us-east5"}, -1},
		{"different project", TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "global", Backend: "other-project"}, -1},
		{"direct provider", TargetConfig{Provider: modelrouter.ProviderAnthropic, Model: "claude-sonnet-4-6"}, -1},
		{"direct ignores region", TargetConfig{Provider: modelrouter.ProviderAnthropic, Model: "claude-sonnet-4-6", Region: "global"}, 5},
		{"different direct account", TargetConfig{Provider: modelrouter.ProviderAnthropic, Model: "claude-sonnet-4-6", Backend: "other-account"}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, selection, err := runtime.Target(tc.config).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if selection.Provider != tc.config.Provider || selection.LogicalModel != tc.config.Model {
				t.Fatalf("selection = %+v, want provider/model from %+v", selection, tc.config)
			}
			if tc.sharedWith >= 0 {
				if router != routers[tc.sharedWith] {
					t.Fatal("matching backend did not reuse router")
				}
			} else {
				for _, previous := range routers {
					if router == previous {
						t.Fatal("different backend reused router")
					}
				}
			}
			routers = append(routers, router)
		})
	}
}

func TestRuntimeConcurrentResolutionAndSnapshot(t *testing.T) {
	t.Parallel()
	routes := runtimeRoutes()
	routes[0].Capabilities.Efforts = []effort.Level{effort.Low, effort.Medium}
	backends := []Backend{{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "original-project"}}}
	runtime, err := NewRuntime(routes, backends...)
	if err != nil {
		t.Fatal(err)
	}
	routes[0].ProviderModelID = "mutated"
	routes[0].Capabilities.Efforts[0] = effort.Max
	backends[0].Google.ProjectID = "invalid/project"
	target := runtime.Target(TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "global"})
	var wg sync.WaitGroup
	results := make([]*Router, 20)
	for i := range results {
		wg.Go(func() {
			router, selection, err := target.Resolve()
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = router
			resolution, err := router.Resolve(selection)
			if err != nil {
				t.Error(err)
				return
			}
			if resolution.Plan().ProviderModelID() != "claude-sonnet-4-6" || !resolution.Plan().Capabilities().SupportsEffort(effort.Low) {
				t.Error("caller mutation changed runtime plan")
			}
		})
	}
	wg.Wait()
	for _, router := range results {
		if router == nil || router != results[0] {
			t.Fatal("concurrent resolution did not reuse router")
		}
	}
}

func TestRuntimeInvalidTargets(t *testing.T) {
	t.Parallel()
	runtime, err := NewRuntime(runtimeRoutes(), Backend{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		target Target
		want   error
	}{
		{"zero target", Target{}, ErrInvalidRouter},
		{"zero runtime", new(Runtime).Target(TargetConfig{}), ErrInvalidRouter},
		{"unknown model", runtime.Target(TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-unknown", Region: "global"}), modelrouter.ErrRouteNotFound},
		{"no implicit provider", runtime.Target(TargetConfig{Model: "claude-sonnet-4-6", Region: "global"}), modelrouter.ErrInvalidSelection},
		{"missing backend", runtime.Target(TargetConfig{Provider: modelrouter.ProviderAnthropic, Model: "claude-sonnet-4-6"}), ErrInvalidRouter},
		{"wrong backend", runtime.Target(TargetConfig{Provider: modelrouter.ProviderAnthropic, Model: "claude-sonnet-4-6", Backend: "vertex"}), ErrInvalidRouter},
		{"missing region", runtime.Target(TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6"}), ErrInvalidAdapter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := tc.target.Resolve()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRuntimeRejectsDuplicateConfiguration(t *testing.T) {
	t.Parallel()
	routes := runtimeRoutes()
	vertex := Backend{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project"}}
	for _, tc := range []struct {
		name     string
		routes   []modelrouter.Route
		backends []Backend
		want     error
	}{
		{"duplicate route", append(routes, routes[0]), []Backend{vertex}, modelrouter.ErrDuplicateRoute},
		{"duplicate backend", routes, []Backend{vertex, vertex}, ErrInvalidRouter},
		{"unsupported provider", routes, []Backend{{Provider: "unsupported"}}, ErrInvalidRouter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRuntime(tc.routes, tc.backends...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewRuntime error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewWithTargetValidatesBeforeAdapterAndDoesNotFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*modelrouter.Route)
		want   error
		calls  int
	}{
		{"tool calling", func(r *modelrouter.Route) { r.Capabilities.ToolCalling = false }, modelrouter.ErrUnsupportedCapability, 0},
		{"terminal submission", func(r *modelrouter.Route) { r.Capabilities.TerminalSubmission = false }, modelrouter.ErrUnsupportedCapability, 0},
		{"adapter failure", func(*modelrouter.Route) {}, context.Canceled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			routes := runtimeRoutes()
			tc.mutate(&routes[0])
			runtime, err := NewRuntime(routes, Backend{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project"}})
			if err != nil {
				t.Fatal(err)
			}
			target := runtime.Target(TargetConfig{Provider: modelrouter.ProviderVertexAI, Model: "claude-sonnet-4-6", Region: "global"})
			router, _, err := target.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			// Replace only the external binding boundary on the real runtime's
			// cached router. All selection and capability checks remain real.
			router.adapters.AnthropicMessages, err = NewAnthropicMessagesAdapterRegistry(AnthropicMessagesRegistration{
				Provider: modelrouter.ProviderVertexAI,
				Adapter: func(context.Context, modelrouter.Plan) (AnthropicMessagesBinding, error) {
					calls++
					return AnthropicMessagesBinding{}, context.Canceled
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = NewWithTarget[*testRequest](t.Context(), target, routedTestConfig(t))
			if !errors.Is(err, tc.want) || calls != tc.calls {
				t.Fatalf("construction = (%v, %d calls), want (%v, %d calls)", err, calls, tc.want, tc.calls)
			}
		})
	}
}

func TestNewWithTargetConstructsSupportedVertexProtocols(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeGoogleCredentials(t))
	routes := []modelrouter.Route{
		vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash"),
		vertexRouterTestRoute(modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6"),
		vertexRouterTestRoute(modelrouter.ProtocolOpenAIChatCompletions, "google/gemini-2.5-flash"),
	}
	runtime, err := NewRuntime(routes, Backend{Provider: modelrouter.ProviderVertexAI, Google: VertexConfig{ProjectID: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		t.Run(string(route.Protocol), func(t *testing.T) {
			agent, err := NewWithTarget[*testRequest](t.Context(), runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel, Region: "global"}), routedTestConfig(t))
			if err != nil || agent == nil {
				t.Fatalf("construction = (%v, %v), want agent", agent, err)
			}
		})
	}
}
