/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package metaagent

import (
	"errors"
	"strings"
	"testing"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/modelrouter"
)

func runtimeBedrockRoutes() []modelrouter.Route {
	chat := bedrockChatRoute()
	claude := chat
	claude.Protocol = modelrouter.ProtocolAnthropicMessages
	claude.Selection.LogicalModel = "claude-sonnet-4-6"
	claude.ProviderModelID = "anthropic.claude-sonnet-4-6"
	responses := chat
	responses.Protocol = modelrouter.ProtocolOpenAIResponses
	responses.Selection.LogicalModel = "gpt-5.6-sol"
	return []modelrouter.Route{claude, chat, responses}
}

func TestBedrockRuntimeTargets(t *testing.T) {
	// These are protocol fixtures, not claims of model entitlement. No AWS
	// credential discovery may happen until a selected adapter is bound.
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-load")
	routes := runtimeBedrockRoutes()
	runtime, err := NewRuntime(routes,
		Backend{Provider: modelrouter.ProviderAWSBedrock, AWS: awsauth.Config{Region: "us-east-1", Profile: "first"}},
		Backend{Name: "other", Provider: modelrouter.ProviderAWSBedrock, AWS: awsauth.Config{Region: "us-east-1", Profile: "second"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var first *Router
	for _, route := range routes {
		t.Run(string(route.Protocol), func(t *testing.T) {
			target := runtime.Target(TargetConfig{Provider: modelrouter.ProviderAWSBedrock, Model: route.Selection.LogicalModel})
			router, selection, err := target.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = router
			} else if first != router {
				t.Fatal("models on the same AWS backend did not share a router")
			}
			resolution, err := router.Resolve(selection)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Plan().ProviderModelID() != route.ProviderModelID {
				t.Fatal("wire model ID changed")
			}
			// Exercise the actual registered adapter: binding must reach AWS auth
			// and reject the static credential, rather than use another provider.
			_, err = NewWithTarget[*testRequest](t.Context(), target, routedTestConfig(t))
			if err == nil || !strings.Contains(err.Error(), "bedrock runtime: credential validation failed") {
				t.Fatalf("binding error = %v, want Bedrock credential validation failure", err)
			}
		})
	}
	for _, tc := range []struct {
		name, backend, region string
		shared                bool
	}{
		{"explicit default region", "", "us-east-1", true},
		{"different region", "", "us-west-2", false},
		{"different account", "other", "us-east-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, _, err := runtime.Target(TargetConfig{Provider: modelrouter.ProviderAWSBedrock, Model: routes[0].Selection.LogicalModel, Backend: tc.backend, Region: tc.region}).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if (router == first) != tc.shared {
				t.Fatalf("router sharing = %v, want %v", router == first, tc.shared)
			}
		})
	}
}

func TestBedrockRuntimeRouterRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	routes := runtimeBedrockRoutes()
	wrongProvider := routes[0]
	wrongProvider.Selection.Provider = modelrouter.ProviderVertexAI
	unsupported := routes[0]
	unsupported.Protocol = modelrouter.ProtocolGoogleGenAI
	for _, tc := range []struct {
		name   string
		cfg    awsauth.Config
		routes []modelrouter.Route
	}{
		{"no routes", awsauth.Config{Region: "us-east-1"}, nil},
		{"no region", awsauth.Config{}, routes},
		{"invalid region", awsauth.Config{Region: "global"}, routes},
		{"invalid profile", awsauth.Config{Region: "us-east-1", Profile: " spaced "}, routes},
		{"foreign provider", awsauth.Config{Region: "us-east-1"}, []modelrouter.Route{wrongProvider}},
		{"unsupported protocol", awsauth.Config{Region: "us-east-1"}, []modelrouter.Route{unsupported}},
		{"duplicate route", awsauth.Config{Region: "us-east-1"}, []modelrouter.Route{routes[0], routes[0]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewBedrockRuntimeRouter(tc.cfg, tc.routes...); err == nil {
				t.Fatal("invalid configuration succeeded")
			}
		})
	}
}

func TestBedrockTargetChecksCapabilitiesBeforeCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-load")
	route := runtimeBedrockRoutes()[0]
	route.Capabilities.ToolCalling = false
	runtime, err := NewRuntime([]modelrouter.Route{route}, Backend{Provider: modelrouter.ProviderAWSBedrock, AWS: awsauth.Config{Region: "us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewWithTarget[*testRequest](t.Context(), runtime.Target(TargetConfig{Provider: modelrouter.ProviderAWSBedrock, Model: route.Selection.LogicalModel}), routedTestConfig(t))
	if !errors.Is(err, modelrouter.ErrUnsupportedCapability) {
		t.Fatalf("construction error = %v, want unsupported capability", err)
	}
}
