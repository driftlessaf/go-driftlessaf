/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/agenttrace"
	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/executor/systemone"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

type testSystemOneService struct{}

func (testSystemOneService) Ask(_ context.Context, req systemone.Request) (*systemone.Response, error) {
	return &systemone.Response{Model: req.Model, Answers: map[string]systemone.Answer{"post": systemone.NoulAnswer{Probability: 0.8}}}, nil
}

func TestServingBackendSupportsChatAndSystemOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		provider    modelrouter.Provider
		attribution modelrouter.Attribution
		chat        modelrouter.Route
		backend     Backend
	}{
		{
			name: "Vertex", provider: modelrouter.ProviderVertexAI,
			attribution: modelrouter.Attribution{ProviderName: "gcp.vertex_ai", LegacySystem: agenttrace.SystemGoogleVertex},
			chat:        vertexRouterTestRoute(modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash"),
			backend: VertexBackend("", VertexConfig{ProjectID: "project"}, WithSystemOneAdapter(func(_ context.Context, plan modelrouter.Plan) (SystemOneBinding, error) {
				return NewSystemOneBinding(plan, testSystemOneService{}, nil)
			})),
		},
		{
			name: "Bedrock", provider: modelrouter.ProviderAWSBedrock,
			attribution: modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock},
			chat: modelrouter.Route{
				Selection: modelrouter.Selection{Provider: modelrouter.ProviderAWSBedrock, LogicalModel: "claude-sonnet-4-6"},
				Protocol:  modelrouter.ProtocolAnthropicMessages, ProviderModelID: "us.anthropic.claude-sonnet-4-6",
				Attribution: modelrouter.Attribution{ProviderName: agenttrace.SystemBedrock, LegacySystem: agenttrace.SystemBedrock},
			},
			backend: BedrockBackend("", awsauth.Config{Region: "us-east-1"}, WithSystemOneAdapter(func(_ context.Context, plan modelrouter.Plan) (SystemOneBinding, error) {
				return NewSystemOneBinding(plan, testSystemOneService{}, nil)
			})),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			systemOne := modelrouter.Route{
				Selection: modelrouter.Selection{Provider: tc.provider, LogicalModel: systemone.ModelHopper},
				Protocol:  modelrouter.ProtocolSystemOne, ProviderModelID: "served-hopper",
				Attribution: tc.attribution,
			}
			runtime, err := NewRuntime([]modelrouter.Route{tc.chat, systemOne}, tc.backend)
			if err != nil {
				t.Fatal(err)
			}
			region := "global"
			if tc.provider == modelrouter.ProviderAWSBedrock {
				region = "us-east-1"
			}
			for _, selection := range []modelrouter.Selection{tc.chat.Selection, systemOne.Selection} {
				router, selected, err := runtime.Target(TargetConfig{Provider: selection.Provider, Model: selection.LogicalModel, Region: region}).Resolve()
				if err != nil {
					t.Fatal(err)
				}
				resolution, err := router.Resolve(selected)
				if err != nil {
					t.Fatal(err)
				}
				if selection == systemOne.Selection {
					binding, err := resolution.BindSystemOne(t.Context(), modelrouter.Requirements{})
					if err != nil {
						t.Fatal(err)
					}
					response, err := binding.Service().Ask(t.Context(), systemone.Request{Model: binding.Plan().ProviderModelID()})
					if err != nil {
						t.Fatal(err)
					}
					if response == nil || response.Model != "served-hopper" {
						t.Errorf("System One response: got = (%v, %v), want model served-hopper", response, err)
					}
					if _, err := binding.Service().Ask(t.Context(), systemone.Request{Model: systemone.ModelJevLatest}); !errors.Is(err, systemone.ErrInvalidRequest) {
						t.Errorf("cross-model request: got = %v, want ErrInvalidRequest", err)
					}
				} else if got := resolution.Plan().Protocol(); got != tc.chat.Protocol {
					t.Errorf("chat protocol: got = %q, want = %q", got, tc.chat.Protocol)
				}
			}
		})
	}
}

func TestSystemOneBindingRejectsMissingAdapter(t *testing.T) {
	t.Parallel()
	route := modelrouter.Route{
		Selection: modelrouter.Selection{Provider: "baseten", LogicalModel: systemone.ModelHopper},
		Protocol:  modelrouter.ProtocolSystemOne, ProviderModelID: systemone.ModelHopper,
		Attribution: modelrouter.Attribution{ProviderName: "baseten", LegacySystem: "baseten"},
	}
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouterWithAdapters(registry, AdapterRegistrations{})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := router.Resolve(route.Selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolution.BindSystemOne(t.Context(), modelrouter.Requirements{}); !errors.Is(err, ErrAdapterNotFound) {
		t.Errorf("BindSystemOne: got = %v, want ErrAdapterNotFound", err)
	}
}

func TestSystemOneBindingRejectsSubstitutedPlan(t *testing.T) {
	t.Parallel()
	routes := []modelrouter.Route{
		{
			Selection: modelrouter.Selection{Provider: "baseten", LogicalModel: systemone.ModelHopper},
			Protocol:  modelrouter.ProtocolSystemOne, ProviderModelID: systemone.ModelHopper,
			Attribution: modelrouter.Attribution{ProviderName: "baseten", LegacySystem: "baseten"},
		},
		{
			Selection: modelrouter.Selection{Provider: "baseten", LogicalModel: systemone.ModelJevLatest},
			Protocol:  modelrouter.ProtocolSystemOne, ProviderModelID: systemone.ModelJevLatest,
			Attribution: modelrouter.Attribution{ProviderName: "baseten", LegacySystem: "baseten"},
		},
	}
	registry, err := modelrouter.NewRegistry(routes...)
	if err != nil {
		t.Fatal(err)
	}
	otherPlan, err := registry.Resolve(routes[1].Selection)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouterWithAdapters(registry, AdapterRegistrations{SystemOne: []SystemOneRegistration{{
		Provider: "baseten", Adapter: func(context.Context, modelrouter.Plan) (SystemOneBinding, error) {
			return NewSystemOneBinding(otherPlan, testSystemOneService{}, nil)
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := router.Resolve(routes[0].Selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolution.BindSystemOne(t.Context(), modelrouter.Requirements{}); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("BindSystemOne: got = %v, want ErrInvalidBinding", err)
	}
}
