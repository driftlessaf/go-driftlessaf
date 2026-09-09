/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chainguard.dev/driftlessaf/agents/modelrouter"
	"chainguard.dev/driftlessaf/agents/toolcall"
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
	for _, feature := range []string{"thinking", "suspend", "refusal", "schema", "request timeout", "execution timeout"} {
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
			case "request timeout":
				cfg.ResponsesRequestTimeout = -time.Second
			case "execution timeout":
				cfg.ResponsesExecutionTimeout = -time.Second
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

func TestRoutedResponsesTimeouts(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"request", "execution", "other protocol"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "fixture rejection", http.StatusBadRequest)
			}))
			defer server.Close()
			observed := false
			service := responses.NewResponseService(option.WithBaseURL(server.URL), option.WithHTTPClient(server.Client()), option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
				observed = true
				deadline, ok := r.Context().Deadline()
				if remaining := time.Until(deadline); !ok || remaining > 10*time.Minute || remaining < 9*time.Minute {
					t.Errorf("request deadline: got = %v, want approximately 10m", remaining)
				}
				return next(r)
			}))
			registry, err := NewOpenAIResponsesAdapterRegistry(OpenAIResponsesRegistration{
				Provider: modelrouter.ProviderAWSBedrock,
				Adapter: func(_ context.Context, plan modelrouter.Plan) (OpenAIResponsesBinding, error) {
					return NewOpenAIResponsesBinding(plan, service, nil)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			route := responsesRoute()
			cfg := routedTestConfig(t)
			if kind == "execution" {
				cfg.ResponsesExecutionTimeout = 10 * time.Minute
			} else {
				cfg.ResponsesRequestTimeout = 10 * time.Minute
			}
			if kind == "other protocol" {
				route.Protocol = modelrouter.ProtocolOpenAIChatCompletions
			}
			router := mustRouter(t, mustRouteRegistry(t, route), AdapterRegistries{OpenAIResponses: registry})
			agent, err := NewRouted[*testRequest](t.Context(), router, route.Selection, cfg)
			if kind == "other protocol" {
				if err == nil || errors.Is(err, ErrAdapterNotFound) {
					t.Fatalf("error: got = %v, want timeout rejection before binding", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Execute(t.Context(), &testRequest{}, toolcall.EmptyTools{}); err == nil {
				t.Fatal("fixture HTTP rejection was ignored")
			}
			if !observed {
				t.Fatal("request middleware was not invoked")
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
