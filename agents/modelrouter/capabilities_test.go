/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"errors"
	"testing"

	"chainguard.dev/driftlessaf/agents/effort"
	"chainguard.dev/driftlessaf/agents/modelrouter"
	"github.com/google/go-cmp/cmp"
)

func TestEffectiveCapabilities(t *testing.T) {
	t.Parallel()

	allEfforts := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}
	tests := []struct {
		name  string
		route modelrouter.Route
		want  modelrouter.Capabilities
	}{
		{
			name:  "Gemini budget model",
			route: newRoute(modelrouter.ProviderVertexAI, "gemini-2.5-flash", modelrouter.ProtocolGoogleGenAI, "gemini-2.5-flash"),
			want: modelrouter.Capabilities{
				Efforts:                allEfforts,
				ExplicitThinkingBudget: true,
				SamplingParameters:     true,
				ToolCalling:            true,
				TerminalSubmission:     true,
				MaximumOutputTokens:    true,
			},
		},
		{
			name:  "Gemini level model has no explicit budget",
			route: newRoute(modelrouter.ProviderVertexAI, "gemini-3-flash-preview", modelrouter.ProtocolGoogleGenAI, "gemini-3-flash-preview"),
			want: modelrouter.Capabilities{
				Efforts:             allEfforts,
				SamplingParameters:  true,
				ToolCalling:         true,
				TerminalSubmission:  true,
				MaximumOutputTokens: true,
			},
		},
		{
			name:  "adaptive Claude model",
			route: newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5"),
			want: modelrouter.Capabilities{
				Efforts:             allEfforts,
				PromptCaching:       true,
				ToolCalling:         true,
				TerminalSubmission:  true,
				SuspendResume:       true,
				MaximumOutputTokens: true,
				RefusalRecovery:     true,
			},
		},
		{
			name:  "Claude model without effort",
			route: newRoute(modelrouter.ProviderAWSBedrock, "claude-sonnet-4-5", modelrouter.ProtocolAnthropicMessages, "anthropic.claude-sonnet-4-5"),
			want: modelrouter.Capabilities{
				Efforts:                []effort.Level{},
				ExplicitThinkingBudget: true,
				SamplingParameters:     true,
				PromptCaching:          true,
				ToolCalling:            true,
				TerminalSubmission:     true,
				SuspendResume:          true,
				MaximumOutputTokens:    true,
				RefusalRecovery:        true,
			},
		},
		{
			name:  "OpenAI Chat Completions",
			route: newRoute(modelrouter.ProviderVertexAI, "meta/llama-3.3", modelrouter.ProtocolOpenAIChatCompletions, "meta/llama-3.3"),
			want: modelrouter.Capabilities{
				Efforts:             allEfforts,
				SamplingParameters:  true,
				ToolCalling:         true,
				TerminalSubmission:  true,
				MaximumOutputTokens: true,
			},
		},
		{
			name: "exact route allow set",
			route: func() modelrouter.Route {
				route := newRoute(modelrouter.ProviderAWSBedrock, "claude-sonnet-4-6", modelrouter.ProtocolAnthropicMessages, "anthropic.claude-sonnet-4-6")
				route.Capabilities = modelrouter.Capabilities{
					Efforts: []effort.Level{effort.Max, effort.Low},
				}
				return route
			}(),
			want: modelrouter.Capabilities{Efforts: []effort.Level{effort.Low, effort.Max}},
		},
		{
			name: "nil efforts disables effort",
			route: func() modelrouter.Route {
				route := newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
				route.Capabilities.Efforts = nil
				return route
			}(),
			want: modelrouter.Capabilities{
				Efforts:             []effort.Level{},
				PromptCaching:       true,
				ToolCalling:         true,
				TerminalSubmission:  true,
				SuspendResume:       true,
				MaximumOutputTokens: true,
				RefusalRecovery:     true,
			},
		},
		{
			name: "empty efforts disables effort",
			route: func() modelrouter.Route {
				route := newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
				route.Capabilities.Efforts = []effort.Level{}
				return route
			}(),
			want: modelrouter.Capabilities{
				Efforts:             []effort.Level{},
				PromptCaching:       true,
				ToolCalling:         true,
				TerminalSubmission:  true,
				SuspendResume:       true,
				MaximumOutputTokens: true,
				RefusalRecovery:     true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry, err := modelrouter.NewRegistry(tt.route)
			if err != nil {
				t.Fatalf("NewRegistry() error = %v", err)
			}
			plan, err := registry.Resolve(tt.route.Selection)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, plan.Capabilities()); diff != "" {
				t.Errorf("Capabilities() mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestPlanValidateRequirements(t *testing.T) {
	t.Parallel()

	route := newRoute(modelrouter.ProviderVertexAI, "gemini-3-flash-preview", modelrouter.ProtocolGoogleGenAI, "gemini-3-flash-preview")
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if err := plan.ValidateRequirements(modelrouter.Requirements{
		Effort:              effort.XHigh,
		SamplingParameters:  true,
		ToolCalling:         true,
		TerminalSubmission:  true,
		MaximumOutputTokens: true,
	}); err != nil {
		t.Fatalf("ValidateRequirements() supported requirements error = %v", err)
	}

	err = plan.ValidateRequirements(modelrouter.Requirements{
		ExplicitThinkingBudget: true,
		PromptCaching:          true,
		SuspendResume:          true,
		RefusalRecovery:        true,
	})
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("ValidateRequirements() error = %v, want ErrUnsupportedCapability", err)
	}
	want := `route for provider "vertex" and logical model "gemini-3-flash-preview": unsupported route capability: explicit thinking budget, prompt caching, suspend and resume, refusal recovery`
	if got := err.Error(); got != want {
		t.Errorf("ValidateRequirements() error = %q, want %q", got, want)
	}
}

func TestPlanValidateRequirementsRejectsInvalidEffort(t *testing.T) {
	t.Parallel()

	route := newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-5", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-5")
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	err = plan.ValidateRequirements(modelrouter.Requirements{Effort: "turbo"})
	want := `route for provider "anthropic" and logical model "claude-sonnet-5": invalid capability requirement: invalid effort level "turbo" (want low|medium|high|xhigh|max)`
	if err == nil || err.Error() != want {
		t.Fatalf("ValidateRequirements() error = %v, want %q", err, want)
	}
}

func TestPlanValidateRequirementsReportsEveryUnsupportedCapabilityInStableOrder(t *testing.T) {
	t.Parallel()

	route := newRoute(modelrouter.ProviderAnthropic, "claude-sonnet-4-6", modelrouter.ProtocolAnthropicMessages, "claude-sonnet-4-6")
	route.Capabilities = modelrouter.Capabilities{}
	registry, err := modelrouter.NewRegistry(route)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	plan, err := registry.Resolve(route.Selection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	err = plan.ValidateRequirements(modelrouter.Requirements{
		Effort:                 effort.High,
		ExplicitThinkingBudget: true,
		SamplingParameters:     true,
		PromptCaching:          true,
		ToolCalling:            true,
		TerminalSubmission:     true,
		SuspendResume:          true,
		MaximumOutputTokens:    true,
		RefusalRecovery:        true,
	})
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("ValidateRequirements() error = %v, want ErrUnsupportedCapability", err)
	}
	want := `route for provider "anthropic" and logical model "claude-sonnet-4-6": unsupported route capability: effort "high", explicit thinking budget, sampling parameters, prompt caching, tool calling, terminal submission, suspend and resume, maximum output tokens, refusal recovery`
	if got := err.Error(); got != want {
		t.Errorf("ValidateRequirements() error = %q, want %q", got, want)
	}
}
