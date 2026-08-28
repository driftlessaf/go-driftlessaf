/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package modelrouter_test

import (
	"testing"

	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func FuzzNewRegistryNeverPanics(f *testing.F) {
	f.Add("vertex", "claude-sonnet-5", "anthropic-messages", "claude-sonnet-5")
	f.Add("test-provider", "meta/llama", "openai-chat-completions", "model")
	f.Add("", "", "", "")
	f.Fuzz(func(t *testing.T, provider, logicalModel, protocol, providerModelID string) {
		registry, err := modelrouter.NewRegistry(modelrouter.Route{
			Selection: modelrouter.Selection{
				Provider:     modelrouter.Provider(provider),
				LogicalModel: logicalModel,
			},
			Protocol:        modelrouter.Protocol(protocol),
			ProviderModelID: providerModelID,
		})
		if err != nil {
			return
		}
		plan, err := registry.Resolve(modelrouter.Selection{
			Provider:     modelrouter.Provider(provider),
			LogicalModel: logicalModel,
		})
		if err != nil {
			t.Errorf("Resolve() of accepted route error = %v", err)
		}
		if err := plan.Validate(); err != nil {
			t.Errorf("Validate() of resolved plan error = %v", err)
		}
	})
}

func FuzzResolveNeverPanics(f *testing.F) {
	selection := modelrouter.Selection{Provider: modelrouter.ProviderVertexAI, LogicalModel: "claude-sonnet-5"}
	registry, err := modelrouter.NewRegistry(modelrouter.Route{
		Selection:       selection,
		Protocol:        modelrouter.ProtocolAnthropicMessages,
		ProviderModelID: "claude-sonnet-5",
	})
	if err != nil {
		f.Fatalf("NewRegistry() error = %v", err)
	}
	f.Add("vertex", "claude-sonnet-5")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, provider, logicalModel string) {
		_, _ = registry.Resolve(modelrouter.Selection{
			Provider:     modelrouter.Provider(provider),
			LogicalModel: logicalModel,
		})
	})
}
