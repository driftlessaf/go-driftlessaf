/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func TestAdapterBackendResolutionAndBinding(t *testing.T) {
	t.Parallel()
	route := runtimeRoutes()[0]
	route.Selection.Provider = "example-provider"
	var factories, bindings atomic.Int32
	wantErr := errors.New("selected credential boundary")
	backend := NewAdapterBackend("account-one", route.Selection.Provider, func(region string) (AdapterRegistrations, error) {
		factories.Add(1)
		if region != "region-one" && region != "region-two" {
			t.Errorf("region: got = %q", region)
		}
		return AdapterRegistrations{AnthropicMessages: []AnthropicMessagesRegistration{{
			Provider: route.Selection.Provider,
			Adapter: func(context.Context, modelrouter.Plan) (AnthropicMessagesBinding, error) {
				bindings.Add(1)
				return AnthropicMessagesBinding{}, wantErr
			},
		}}}, nil
	})
	runtime, err := NewRuntime([]modelrouter.Route{route}, backend, NewAdapterBackend("other-account", route.Selection.Provider, func(string) (AdapterRegistrations, error) {
		t.Error("unselected account factory invoked")
		return AdapterRegistrations{}, wantErr
	}))
	if err != nil {
		t.Fatal(err)
	}
	if factories.Load() != 0 || bindings.Load() != 0 {
		t.Fatal("startup invoked factory or binding")
	}
	target := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel, Region: "region-one", Backend: "account-one"})
	unknown := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: "unknown", Backend: "account-one"})
	if _, _, err := unknown.Resolve(); !errors.Is(err, modelrouter.ErrRouteNotFound) || factories.Load() != 0 {
		t.Fatalf("undeclared selection: got = %v with %d factory calls", err, factories.Load())
	}
	routers := make([]*Router, 20)
	var wg sync.WaitGroup
	for i := range routers {
		wg.Go(func() {
			router, _, err := target.Resolve()
			if err != nil {
				t.Error(err)
				return
			}
			routers[i] = router
		})
	}
	wg.Wait()
	for _, router := range routers {
		if router == nil || router != routers[0] {
			t.Fatal("concurrent callers did not share router")
		}
	}
	if factories.Load() != 1 || bindings.Load() != 0 {
		t.Fatalf("resolution: factories = %d, bindings = %d", factories.Load(), bindings.Load())
	}
	other, _, err := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel, Region: "region-two", Backend: "account-one"}).Resolve()
	if err != nil || other == routers[0] || factories.Load() != 2 {
		t.Fatalf("region isolation: err = %v, factories = %d", err, factories.Load())
	}
	cfg := routedTestConfig(t)
	cfg.Effort = "unknown"
	if _, err := NewWithTarget[*testRequest](t.Context(), target, cfg); err == nil || bindings.Load() != 0 {
		t.Fatalf("invalid requirement: err = %v, bindings = %d", err, bindings.Load())
	}
	if _, err := NewWithTarget[*testRequest](t.Context(), target, routedTestConfig(t)); !errors.Is(err, wantErr) || bindings.Load() != 1 {
		t.Fatalf("selected binding: err = %v, bindings = %d", err, bindings.Load())
	}
}

func TestAdapterBackendFailuresAreNotCached(t *testing.T) {
	t.Parallel()
	route := runtimeRoutes()[0]
	attempts := 0
	want := errors.New("factory failure")
	runtime, err := NewRuntime([]modelrouter.Route{route}, NewAdapterBackend("", route.Selection.Provider, func(string) (AdapterRegistrations, error) {
		attempts++
		if attempts == 1 {
			return AdapterRegistrations{}, want
		}
		return AdapterRegistrations{AnthropicMessages: []AnthropicMessagesRegistration{{Provider: route.Selection.Provider, Adapter: func(context.Context, modelrouter.Plan) (AnthropicMessagesBinding, error) {
			return AnthropicMessagesBinding{}, want
		}}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	target := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel})
	if _, _, err := target.Resolve(); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if _, _, err := target.Resolve(); err != nil || attempts != 2 {
		t.Fatalf("retry: err = %v, attempts = %d", err, attempts)
	}
}

func TestAdapterBackendRejectsWrongProvider(t *testing.T) {
	t.Parallel()
	route := runtimeRoutes()[0]
	runtime, err := NewRuntime([]modelrouter.Route{route}, NewAdapterBackend("", route.Selection.Provider, func(string) (AdapterRegistrations, error) {
		return AdapterRegistrations{AnthropicMessages: []AnthropicMessagesRegistration{{Provider: modelrouter.ProviderAnthropic, Adapter: func(context.Context, modelrouter.Plan) (AnthropicMessagesBinding, error) {
			t.Error("wrong provider bound")
			return AnthropicMessagesBinding{}, nil
		}}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.Target(TargetConfig{Provider: route.Selection.Provider, Model: route.Selection.LogicalModel}).Resolve(); !errors.Is(err, ErrInvalidRouter) {
		t.Fatalf("provider mismatch: got = %v", err)
	}
	if _, err := NewRuntime([]modelrouter.Route{route}, NewAdapterBackend("", route.Selection.Provider, nil)); !errors.Is(err, ErrInvalidRouter) {
		t.Fatalf("nil factory: got = %v", err)
	}
}
