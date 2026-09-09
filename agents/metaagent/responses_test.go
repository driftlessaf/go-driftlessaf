/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

func responsesRoute() modelrouter.Route {
	r := bedrockChatRoute()
	r.Protocol = modelrouter.ProtocolOpenAIResponses
	return r
}

func TestResponsesRequirementsPrecedeAdapter(t *testing.T) {
	t.Parallel()
	for _, feature := range []string{"thinking", "suspend", "refusal", "schema"} {
		t.Run(feature, func(t *testing.T) {
			t.Parallel()
			calls := 0
			registry, err := NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{Provider: modelrouter.ProviderAWSBedrock, Adapter: func(context.Context, modelrouter.Plan) (OpenAIResponsesBinding, error) {
				calls++
				return OpenAIResponsesBinding{}, errors.New("must not bind")
			}})
			if err != nil {
				t.Fatal(err)
			}
			route := responsesRoute()
			router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry})
			cfg := routedTestConfig(t)
			switch feature {
			case "thinking":
				cfg.ThinkingBudget = 1024
			case "suspend":
				cfg.SuspendToolName = "pause"
			case "refusal":
				cfg.RefusalNudgeMaxRetries = 1
			case "schema":
				cfg.OmitResultSchemaFields = []string{"nonexistent"}
			}
			if _, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg); err == nil {
				t.Error("unsupported config accepted")
			}
			if calls != 0 {
				t.Errorf("adapter invoked %d times", calls)
			}
		})
	}
}

func TestResponsesBindingProvenance(t *testing.T) {
	t.Parallel()
	route := responsesRoute()
	foreign := bedrockChatPlan(t, route)
	for _, kind := range []string{"zero", "foreign", "valid", "missing"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var registry *OpenAIResponsesAdapterRegistry
			if kind != "missing" {
				var err error
				registry, err = NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{Provider: route.Selection.Provider, Adapter: func(_ context.Context, p modelrouter.Plan) (OpenAIResponsesBinding, error) {
					if kind == "zero" {
						return OpenAIResponsesBinding{}, nil
					}
					if kind == "foreign" {
						p = foreign
					}
					return NewOpenAIResponsesBinding(p, responses.NewResponseService(), nil)
				}})
				if err != nil {
					t.Fatal(err)
				}
			}
			router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry})
			_, err := NewRouted[*testRequest](t.Context(), router, route.Selection, routedTestConfig(t))
			switch kind {
			case "valid":
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				if !errors.Is(err, ErrAdapterNotFound) {
					t.Fatal(err)
				}
			default:
				if !errors.Is(err, ErrInvalidBinding) {
					t.Fatalf("err=%v", err)
				}
			}
		})
	}
}

func TestResponsesBindingCopiesAndRegistryValidation(t *testing.T) {
	t.Parallel()
	plan := bedrockChatPlan(t, responsesRoute())
	labels := map[string]string{"region": "fixture"}
	service := responses.NewResponseService(option.WithMaxRetries(0))
	b, err := NewOpenAIResponsesBinding(plan, service, labels)
	if err != nil {
		t.Fatal(err)
	}
	labels["region"] = "changed"
	b.ResourceLabels()["region"] = "changed again"
	service.Options[0] = nil
	copy := b.Responses()
	copy.Options[0] = nil
	if b.ResourceLabels()["region"] != "fixture" || b.Responses().Options[0] == nil {
		t.Error("binding aliased mutable input")
	}
	adapter := func(context.Context, modelrouter.Plan) (OpenAIResponsesBinding, error) { return b, nil }
	for _, tc := range []struct {
		registrations []OpenAIResponsesRegistration
		want          error
	}{
		{[]OpenAIResponsesRegistration{{Provider: "INVALID", Adapter: adapter}}, ErrInvalidAdapter},
		{[]OpenAIResponsesRegistration{{Provider: "bedrock"}}, ErrInvalidAdapter},
		{[]OpenAIResponsesRegistration{{Provider: "bedrock", Adapter: adapter}, {Provider: "bedrock", Adapter: adapter}}, ErrDuplicateAdapter},
	} {
		if _, err := NewOpenAIResponsesAdapterRegistry(tc.registrations...); !errors.Is(err, tc.want) {
			t.Errorf("err=%v want=%v", err, tc.want)
		}
	}
	if _, err := NewOpenAIResponsesBinding(modelrouter.Plan{}, service, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Error(err)
	}
	if _, err := (RouteResolution{}).bindOpenAIResponses(t.Context(), modelrouter.Requirements{}); !errors.Is(err, ErrInvalidRouter) {
		t.Error(err)
	}
}
