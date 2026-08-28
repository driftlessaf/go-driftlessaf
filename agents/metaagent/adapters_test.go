/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/executor/openaiexecutor"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
)

func TestBindingKeepsPlanAuthoritativeAndCopiesLabels(t *testing.T) {
	t.Parallel()

	route := routedTestRoute(
		modelrouter.Selection{Provider: "test-provider", LogicalModel: "test/model"},
		modelrouter.ProtocolOpenAIChatCompletions,
		"opaque-deployment",
	)
	registry := mustRouteRegistry(t, route)
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	labels := map[string]string{"model_name": "opaque-deployment"}
	binding, err := NewOpenAIChatCompletionsBinding(plan, openai.Client{}, openaiexecutor.TokenLimitMaxTokens, labels)
	if err != nil {
		t.Fatalf("NewOpenAIChatCompletionsBinding: %v", err)
	}
	labels["model_name"] = "mutated-input"
	got := binding.ResourceLabels()
	got["model_name"] = "mutated-accessor"
	if got := binding.ResourceLabels()["model_name"]; got != "opaque-deployment" {
		t.Errorf("ResourceLabels model_name = %q, want copied original", got)
	}
	if !binding.Plan().SameResolution(plan) {
		t.Errorf("binding Plan differs from resolved Plan")
	}
	if got := binding.TokenLimitParameter(); got != openaiexecutor.TokenLimitMaxTokens {
		t.Errorf("TokenLimitParameter = %q, want %q", got, openaiexecutor.TokenLimitMaxTokens)
	}
}

func TestBindingConstructorsRejectInvalidPlansAndPolicies(t *testing.T) {
	t.Parallel()

	if _, err := NewGoogleGenAIBinding(modelrouter.Plan{}, nil, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("NewGoogleGenAIBinding zero plan error = %v, want ErrInvalidBinding", err)
	}

	route := routedTestRoute(
		modelrouter.Selection{Provider: "test-provider", LogicalModel: "test/model"},
		modelrouter.ProtocolOpenAIChatCompletions,
		"opaque-deployment",
	)
	registry := mustRouteRegistry(t, route)
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := NewAnthropicMessagesBinding(plan, anthropic.MessageService{}, nil); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("NewAnthropicMessagesBinding protocol mismatch error = %v, want ErrInvalidBinding", err)
	}
	if _, err := NewOpenAIChatCompletionsBinding(plan, openai.Client{}, "output_tokens", nil); !errors.Is(err, ErrInvalidBinding) {
		t.Errorf("NewOpenAIChatCompletionsBinding policy error = %v, want ErrInvalidBinding", err)
	}
}

func TestNewRouterRejectsNilRouteRegistry(t *testing.T) {
	t.Parallel()

	if _, err := NewRouter(nil, AdapterRegistries{}); !errors.Is(err, ErrInvalidRouter) {
		t.Fatalf("NewRouter error = %v, want ErrInvalidRouter", err)
	}
}
